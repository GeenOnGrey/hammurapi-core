package signing

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"status":"success"}`)
	h := http.Header{}
	SetHeaders(h, "old", body, now)
	// HOOK-05: during rotation both secrets are accepted.
	if !Verify(h, body, []string{"new", "old"}, now) {
		t.Fatal("rotated secret rejected")
	}
	// HOOK-01: wrong secret or tampered body.
	if Verify(h, body, []string{"new"}, now) || Verify(h, []byte(`{}`), []string{"old"}, now) {
		t.Fatal("bad signature accepted")
	}
	// HOOK-02: a timestamp older than five minutes is rejected.
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10))
	h.Set(HeaderSignature, Sign("old", body))
	if Verify(h, body, []string{"old"}, now) {
		t.Fatal("stale timestamp accepted")
	}
}

func TestRotate(t *testing.T) {
	if got := Rotate([]string{"a", "b"}, "c"); len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Fatalf("got %v", got)
	}
}
