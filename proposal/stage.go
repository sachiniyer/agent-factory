package proposal

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
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
	for _, d := range s.diskDocs(include) {
		slug, file, ok := isDocPath(d)
		if !ok || !inScope(slug, file) {
			continue
		}
		data, err := s.readDoc(slug, file)
		if err != nil {
			continue
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

// trackedDocs maps each doc path in the index to its blob id.
func (s *Store) trackedDocs() (map[string]string, error) {
	out, err := s.git(nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	docs := map[string]string{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[0] != "100644" {
			continue
		}
		if _, _, ok := isDocPath(path); ok {
			docs[path] = fields[1]
		}
	}
	return docs, nil
}

// diskDocs lists the doc paths on disk under the slugs that include names.
func (s *Store) diskDocs(include []docSpec) []string {
	var slugs []string
	for _, in := range include {
		if in.slug == "" {
			slugs, _ = s.Slugs()
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
		docs, _ := s.Docs(slug)
		for _, d := range docs {
			out = append(out, slug+"/"+d)
		}
	}
	return out
}

func blobID(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
