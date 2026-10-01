package attachments

import (
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/config"
)

func allowed() map[string]bool {
	m := map[string]bool{}
	for _, t := range config.DefaultUploadTypes {
		m[t] = true
	}
	return m
}

func TestDetectAllowed(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	if typ, ok := DetectAllowed(png, allowed()); !ok || typ != "image/png" {
		t.Fatalf("png: %s %v", typ, ok)
	}
	if typ, ok := DetectAllowed([]byte("%PDF-1.7\n1 0 obj\n"), allowed()); !ok || typ != "application/pdf" {
		t.Fatalf("pdf: %s %v", typ, ok)
	}
	if _, ok := DetectAllowed([]byte("plain notes\n"), allowed()); !ok {
		t.Fatal("text rejected")
	}
	// ATT-03: an executable renamed to .pdf is detected by content.
	exe := append([]byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff\x00\x00"), make([]byte, 64)...)
	if typ, ok := DetectAllowed(exe, allowed()); ok {
		t.Fatalf("exe accepted as %s", typ)
	}
}
