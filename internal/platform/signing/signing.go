// Package signing verifies and creates the HMAC signatures of Hammurapi's own
// webhooks (CI results, deploy results, feature flags; outgoing deploy
// triggers): X-Hammurapi-Signature: sha256=<hex HMAC of the body> and
// X-Hammurapi-Timestamp (Unix seconds) not older than five minutes.
package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Headers.
const (
	HeaderSignature = "X-Hammurapi-Signature"
	HeaderTimestamp = "X-Hammurapi-Timestamp"
)

// MaxSkew is the accepted age of a signed request.
const MaxSkew = 5 * time.Minute

// Sign returns the signature header value for body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// SetHeaders signs an outgoing request.
func SetHeaders(h http.Header, secret string, body []byte, now time.Time) {
	h.Set(HeaderSignature, Sign(secret, body))
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
}

// Verify checks the signature against any of the active secrets (two during
// rotation) and the timestamp window.
func Verify(h http.Header, body []byte, secrets []string, now time.Time) bool {
	ts, err := strconv.ParseInt(h.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(ts, 0))
	if age > MaxSkew || age < -MaxSkew {
		return false
	}
	sig := strings.TrimSpace(h.Get(HeaderSignature))
	if !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil {
		return false
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(s))
		mac.Write(body)
		if hmac.Equal(mac.Sum(nil), got) {
			return true
		}
	}
	return false
}
