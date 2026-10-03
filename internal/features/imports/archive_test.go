package imports

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
)

type entry struct {
	name    string
	content string
	mode    fs.FileMode
}

func makeZip(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var lim = Limits{MaxUncompressed: 1 << 20, MaxFiles: 50}

func code(err error) string {
	if e, ok := apperr.As(err); ok {
		return e.Code
	}
	return ""
}

// IMP-01: a feature with five areas and assets.
func TestParseFeatureWithFiveAreas(t *testing.T) {
	var es []entry
	for _, a := range []string{"product", "design", "arch", "tech", "qa"} {
		es = append(es, entry{name: "specs/HMR/CMN/FTR.HMR.CMN-0001/" + a + "/spec.md", content: "# Hammurapi\n"})
	}
	es = append(es, entry{name: "specs/HMR/CMN/FTR.HMR.CMN-0001/design/logo.png", content: "\x89PNG\r\n\x1a\n"})
	a, err := ParseArchive(makeZip(t, es...), lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Features) != 1 {
		t.Fatalf("features = %d", len(a.Features))
	}
	f := a.Features[0]
	if f.ArchiveID != "FTR.HMR.CMN-0001" || f.Domain != "HMR" || f.System != "CMN" {
		t.Fatalf("unexpected feature %+v", f)
	}
	if got := strings.Join(f.Areas(), ","); got != "arch,design,product,qa,tech" {
		t.Fatalf("areas = %s", got)
	}
	if len(f.Files) != 6 {
		t.Fatalf("files = %d", len(f.Files))
	}
}

// IMP-09: path traversal and symlinks fail the whole archive.
func TestParseRejectsUnsafePaths(t *testing.T) {
	cases := []entry{
		{name: "specs/../../etc/passwd", content: "x"},
		{name: "/etc/passwd", content: "x"},
		{name: "specs/A/B/A.B-0001/product/link", content: "/etc/passwd", mode: fs.ModeSymlink | 0o777},
	}
	for _, c := range cases {
		_, err := ParseArchive(makeZip(t, c), lim)
		if code(err) != "unsafe_path" {
			t.Errorf("%s: got %v, want unsafe_path", c.name, err)
		}
	}
}

// IMP-10: the unpacked size is limited even when the header lies.
func TestParseZipBomb(t *testing.T) {
	big := strings.Repeat("0", 2<<20)
	_, err := ParseArchive(makeZip(t, entry{name: "specs/A/B/A.B-0001/product/spec.md", content: big}), lim)
	if code(err) != "too_large_uncompressed" {
		t.Fatalf("got %v", err)
	}
}

func TestParseTooManyFiles(t *testing.T) {
	var es []entry
	for i := 0; i < 3; i++ {
		es = append(es, entry{name: "specs/A/B/A.B-0001/product/f" + string(rune('a'+i)) + ".png", content: "x"})
	}
	_, err := ParseArchive(makeZip(t, es...), Limits{MaxUncompressed: 1 << 20, MaxFiles: 2})
	if code(err) != "too_many_files" {
		t.Fatalf("got %v", err)
	}
}

func TestParseOutsideSpecsRejected(t *testing.T) {
	_, err := ParseArchive(makeZip(t, entry{name: "specs/A/B/A.B-0001/product/spec.md", content: "#"}, entry{name: "README.md", content: "x"}), lim)
	if code(err) != "invalid_paths" {
		t.Fatalf("got %v", err)
	}
}

func TestParseStripsCommonRoot(t *testing.T) {
	a, err := ParseArchive(makeZip(t,
		entry{name: "repo/specs/A/B/A.B-0001/product/spec.md", content: "#"},
		entry{name: "repo/rules/product/template.md", content: "#"}), lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Features) != 1 || len(a.Rules) != 1 {
		t.Fatalf("got %+v", a)
	}
}

func TestParseCorrupt(t *testing.T) {
	_, err := ParseArchive([]byte("not a zip"), lim)
	if code(err) != "archive_corrupt" {
		t.Fatalf("got %v", err)
	}
}
