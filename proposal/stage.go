package proposal

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

const zeroObjectID = "0000000000000000000000000000000000000000"

// docSpec is one parsed pathspec of commitPaths: "." for every doc, a slug,
// or a "slug/file.md" doc.
type docSpec struct{ slug, file string }

func (d docSpec) covers(slug, file string) bool {
	return d.slug == "" || (d.slug == slug && (d.file == "" || d.file == file))
}

func parseSpecs(pathspecs []string) (include, exclude []docSpec) {
	for _, p := range pathspecs {
		target := &include
		if rest, ok := strings.CutPrefix(p, ":(exclude)"); ok {
			target, p = &exclude, rest
		}
		p = strings.TrimSuffix(p, "/")
		if p == "." {
			*target = append(*target, docSpec{})
			continue
		}
		slug, file, _ := strings.Cut(p, "/")
		*target = append(*target, docSpec{slug: slug, file: file})
	}
	return include, exclude
}

// isDocPath reports whether path has the shape slug/name.md of a proposal doc.
func isDocPath(path string) (slug, file string, ok bool) {
	slug, file, found := strings.Cut(path, "/")
	base, md := strings.CutSuffix(file, ".md")
	if !found || !md || !namePattern.MatchString(slug) || !namePattern.MatchString(base) {
		return "", "", false
	}
	return slug, file, true
}

// stage records in the index the docs that pathspecs name, as they are on
// disk. af reads each doc itself without following links and hands git only
// blobs, so git never scans the docs folder: a .gitignore, .gitattributes or
// nested repository that an agent writes there cannot hide a doc, filter one,
// or reach the history.
func (s *Store) stage(pathspecs []string) error {
	include, exclude := parseSpecs(pathspecs)
	inScope := func(slug, file string) bool {
		for _, e := range exclude {
			if e.covers(slug, file) {
				return false
			}
		}
		for _, in := range include {
			if in.covers(slug, file) {
				return true
			}
		}
		return false
	}
	tracked, err := s.trackedDocs()
	if err != nil {
		return err
	}
	var info strings.Builder
	present := map[string]bool{}
	onDisk, err := s.diskDocs(include)
	if err != nil {
		return err
	}
	for _, d := range onDisk {
		slug, file, ok := isDocPath(d)
		if !ok || !inScope(slug, file) {
			continue
		}
		data, err := s.readDoc(slug, file)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errSymlink) {
			continue
		}
		if err != nil {
			return err
		}
		present[d] = true
		if tracked[d] == blobID(data) {
			continue
		}
		id, err := s.git(data, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return err
		}
		fmt.Fprintf(&info, "100644 %s\t%s\x00", strings.TrimSpace(id), d)
	}
	for d := range tracked {
		slug, file, _ := isDocPath(d)
		if !present[d] && inScope(slug, file) {
			fmt.Fprintf(&info, "0 %s\t%s\x00", zeroObjectID, d)
		}
	}
	if info.Len() == 0 {
		return nil
	}
	_, err = s.git([]byte(info.String()), "update-index", "-z", "--index-info")
	return err
}

// changedDocs lists the docs that differ between commits from and to.
func (s *Store) changedDocs(from, to string) ([]docSpec, error) {
	out, err := s.git(nil, "diff", "--name-only", "--no-renames", "-z", from, to, "--")
	if err != nil {
		return nil, err
	}
	var docs []docSpec
	for _, p := range strings.Split(out, "\x00") {
		if slug, file, ok := isDocPath(p); ok {
			docs = append(docs, docSpec{slug: slug, file: file})
		}
	}
	return docs, nil
}

// trackedDocs maps each doc path in the index to its blob id. An entry of any
// other mode than 100644 (one a legacy store imported) maps to "", so stage
// rewrites it as a regular doc or deletes it.
func (s *Store) trackedDocs() (map[string]string, error) {
	out, err := s.git(nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	docs := map[string]string{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			continue
		}
		if _, _, ok := isDocPath(path); !ok {
			continue
		}
		if fields[0] == "100644" {
			docs[path] = fields[1]
		} else {
			docs[path] = ""
		}
	}
	return docs, nil
}

// diskDocs lists the doc paths on disk under the slugs that include names.
// A folder that is gone or is a link holds no docs; any other listing error
// fails, so an unreadable folder is not staged as deleted.
func (s *Store) diskDocs(include []docSpec) ([]string, error) {
	var slugs []string
	for _, in := range include {
		if in.slug == "" {
			all, err := s.Slugs()
			if err != nil {
				return nil, err
			}
			slugs = all
			break
		}
		slugs = append(slugs, in.slug)
	}
	var out []string
	seen := map[string]bool{}
	for _, slug := range slugs {
		if seen[slug] {
			continue
		}
		seen[slug] = true
		docs, err := s.Docs(slug)
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errSymlink) {
			return nil, err
		}
		for _, d := range docs {
			out = append(out, slug+"/"+d)
		}
	}
	return out, nil
}

func blobID(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
