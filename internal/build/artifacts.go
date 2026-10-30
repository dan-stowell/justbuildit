package build

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"strings"
)

// Artifact is one regular file in a build's artifacts tarball.
type Artifact struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// maxListedArtifacts bounds the artifact listing in a result.
const maxListedArtifacts = 1000

// entryName normalizes a tar entry name, returning "" for names that are not
// plain workspace-relative paths.
func entryName(name string) string {
	name = strings.TrimPrefix(name, "./")
	c, err := CleanPath(name)
	if err != nil {
		return ""
	}
	return c
}

// ListArtifacts reads the headers of a gzipped tar and returns its regular
// files. A missing file yields no artifacts and no error.
func ListArtifacts(tarPath string) ([]Artifact, error) {
	f, err := os.Open(tarPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var out []Artifact
	for len(out) < maxListedArtifacts {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if name := entryName(h.Name); name != "" {
			out = append(out, Artifact{Path: name, Size: h.Size})
		}
	}
	return out, nil
}

// OpenArtifact returns a reader for one regular file inside a gzipped tar,
// streamed from the tarball. It returns ErrArtifactNotFound if the tarball or the
// entry does not exist.
func OpenArtifact(tarPath, name string) (io.ReadCloser, int64, error) {
	want, err := CleanPath(strings.TrimPrefix(name, "./"))
	if err != nil {
		return nil, 0, ErrArtifactNotFound
	}
	f, err := os.Open(tarPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrArtifactNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			f.Close()
			return nil, 0, ErrArtifactNotFound
		}
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		if h.Typeflag == tar.TypeReg && entryName(h.Name) == want {
			return struct {
				io.Reader
				io.Closer
			}{tr, f}, h.Size, nil
		}
	}
}
