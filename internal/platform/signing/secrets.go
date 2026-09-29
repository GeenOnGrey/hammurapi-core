package signing

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/GeenOnGrey/hammurapi-core/internal/platform/crypto"
)

// Secrets resolves secret references stored in the database. The database never
// holds a plain secret: a reference is either
//
//	env:NAME        an environment variable (e.g. from a Kubernetes Secret)
//	enc:<base64>    a value generated in the admin panel, encrypted with TOKEN_ENCRYPTION_KEY
type Secrets struct{ box *crypto.Box }

// NewSecrets creates a resolver.
func NewSecrets(box *crypto.Box) *Secrets { return &Secrets{box: box} }

// Resolve returns the secret value of a reference.
func (s *Secrets) Resolve(ref string) (string, error) {
	switch {
	case ref == "":
		return "", errors.New("empty secret reference")
	case strings.HasPrefix(ref, "env:"):
		v := os.Getenv(strings.TrimPrefix(ref, "env:"))
		if v == "" {
			return "", fmt.Errorf("secret %s is not set", ref)
		}
		return v, nil
	case strings.HasPrefix(ref, "enc:"):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ref, "enc:"))
		if err != nil {
			return "", err
		}
		return s.box.Open(raw)
	}
	return "", fmt.Errorf("unsupported secret reference %q", ref)
}

// ResolveAll resolves references, skipping ones that fail.
func (s *Secrets) ResolveAll(refs []string) []string {
	var out []string
	for _, r := range refs {
		if v, err := s.Resolve(r); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// Generate creates a new random secret and returns its value and reference.
func (s *Secrets) Generate() (value, ref string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	value = hex.EncodeToString(b)
	ref, err = s.Encrypt(value)
	return value, ref, err
}

// Encrypt turns a value into an enc: reference.
func (s *Secrets) Encrypt(value string) (string, error) {
	ct, err := s.box.Seal(value)
	if err != nil {
		return "", err
	}
	return "enc:" + base64.StdEncoding.EncodeToString(ct), nil
}

// Rotate keeps at most two refs: the newest first.
func Rotate(refs []string, newRef string) []string {
	out := []string{newRef}
	if len(refs) > 0 {
		out = append(out, refs[0])
	}
	return out
}
