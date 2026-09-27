package imports

import (
	"archive/zip"
	"bytes"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
)

// Limits bound what an archive may contain.
type Limits struct {
	MaxUncompressed int64
	MaxFiles        int
}

// ArchiveFeature is one feature folder specs/<domain>/<system>/<id>/.
type ArchiveFeature struct {
	ArchiveID string
	Domain    string
	System    string
	// Files maps "<area>/<name>" to content.
	Files map[string][]byte
	// Stray lists paths that do not fit the expected layout.
	Stray []string
}

// Areas returns the area folder names in the feature.
func (f *ArchiveFeature) Areas() []string {
	seen := map[string]bool{}
	for p := range f.Files {
		a, _, _ := strings.Cut(p, "/")
		seen[a] = true
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ArchiveRule is a rules/<area>/<file> entry.
type ArchiveRule struct {
	Area    string
	File    string // template.md, fix-template.md or anything else
	Content []byte
}

// Archive is the parsed content of an import archive.
type Archive struct {
	Features []*ArchiveFeature
	Rules    []ArchiveRule
}

// ErrArchive* are archive-level refusals (nothing is imported).
func errArchive(code, msg string) *apperr.Error { return apperr.Unprocessable(code, msg) }

// ParseArchive reads a zip archive in memory. Archive-level problems (corrupt
// file, limits, unsafe paths) fail the whole archive with 422.
func ParseArchive(data []byte, lim Limits) (*Archive, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errArchive("archive_corrupt", "the file is not a valid zip archive")
	}
	var files []*zip.File
	var declared uint64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		files = append(files, f)
		declared += f.UncompressedSize64
	}
	if lim.MaxFiles > 0 && len(files) > lim.MaxFiles {
		return nil, errArchive("too_many_files", "the archive has too many files").With("maxFiles", lim.MaxFiles)
	}
	if lim.MaxUncompressed > 0 && declared > uint64(lim.MaxUncompressed) {
		return nil, errArchive("too_large_uncompressed", "the unpacked archive is too large").With("maxBytes", lim.MaxUncompressed)
	}
	names := make([]string, len(files))
	for i, f := range files {
		n, err := cleanName(f)
		if err != nil {
			return nil, err
		}
		names[i] = n
	}
	strip := commonRoot(names)
	a := &Archive{}
	byID := map[string]*ArchiveFeature{}
	var total int64
	for i, f := range files {
		name := strings.TrimPrefix(names[i], strip)
		if !strings.HasPrefix(name, "specs/") && !strings.HasPrefix(name, "rules/") {
			return nil, errArchive("invalid_paths", "only specs/ and rules/ are allowed in the archive").With("path", name)
		}
		content, err := readLimited(f, lim.MaxUncompressed-total)
		if err != nil {
			return nil, err
		}
		total += int64(len(content))
		parts := strings.Split(name, "/")
		if parts[0] == "rules" {
			if len(parts) != 3 {
				return nil, errArchive("invalid_paths", "rules must be rules/<area>/<file>").With("path", name)
			}
			a.Rules = append(a.Rules, ArchiveRule{Area: parts[1], File: parts[2], Content: content})
			continue
		}
		// specs/<domain>/<system>/<id>/<area>/<file>
		if len(parts) < 4 {
			return nil, errArchive("invalid_paths", "specs must be specs/<domain>/<system>/<id>/<area>/spec.md").With("path", name)
		}
		key := strings.Join(parts[1:4], "/")
		feat := byID[key]
		if feat == nil {
			feat = &ArchiveFeature{ArchiveID: parts[3], Domain: parts[1], System: parts[2], Files: map[string][]byte{}}
			byID[key] = feat
			a.Features = append(a.Features, feat)
		}
		if len(parts) != 6 {
			feat.Stray = append(feat.Stray, strings.Join(parts[4:], "/"))
			continue
		}
		feat.Files[parts[4]+"/"+parts[5]] = content
	}
	sort.Slice(a.Features, func(i, j int) bool {
		x, y := a.Features[i], a.Features[j]
		return x.Domain+x.System+x.ArchiveID < y.Domain+y.System+y.ArchiveID
	})
	return a, nil
}

// cleanName rejects absolute paths, parent references and symlinks.
func cleanName(f *zip.File) (string, error) {
	if f.Mode()&fs.ModeSymlink != 0 {
		return "", errArchive("unsafe_path", "symbolic links are not allowed").With("path", f.Name)
	}
	n := strings.ReplaceAll(f.Name, "\\", "/")
	if strings.HasPrefix(n, "/") || (len(n) > 1 && n[1] == ':') {
		return "", errArchive("unsafe_path", "absolute paths are not allowed").With("path", f.Name)
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", errArchive("unsafe_path", "parent directory references are not allowed").With("path", f.Name)
		}
	}
	c := path.Clean(n)
	if c == "." || strings.HasPrefix(c, "../") {
		return "", errArchive("unsafe_path", "invalid path").With("path", f.Name)
	}
	return c, nil
}

// commonRoot returns "root/" when every entry sits in one top-level folder that
// is not specs/ or rules/ (archives made by zipping a repository folder).
func commonRoot(names []string) string {
	if len(names) == 0 {
		return ""
	}
	first, _, ok := strings.Cut(names[0], "/")
	if !ok || first == "specs" || first == "rules" {
		return ""
	}
	for _, n := range names {
		if !strings.HasPrefix(n, first+"/") {
			return ""
		}
	}
	return first + "/"
}

// readLimited reads an entry, failing when the remaining budget is exceeded
// (the declared size of a zip entry can lie).
func readLimited(f *zip.File, budget int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, errArchive("archive_corrupt", "cannot read "+f.Name)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, budget+1))
	if err != nil {
		return nil, errArchive("archive_corrupt", "cannot read "+f.Name)
	}
	if int64(len(data)) > budget {
		return nil, errArchive("too_large_uncompressed", "the unpacked archive is too large")
	}
	return data, nil
}
