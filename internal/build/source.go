package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"path"
	"sort"
	"strings"
)

// CleanPath validates a workspace-relative path and returns it in canonical
// form. Absolute paths, paths that escape the workspace, and empty paths are
// rejected.
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.ContainsRune(p, 0) || strings.Contains(p, `\`) {
		return "", fmt.Errorf("path %q: contains NUL or backslash", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q: must be relative to the workspace, not absolute", p)
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == ".." {
			return "", fmt.Errorf("path %q: must not contain ..", p)
		}
	}
	c := path.Clean(p)
	if c == "." {
		return "", fmt.Errorf("path %q: does not name a file", p)
	}
	return c, nil
}

// TarFromFiles builds a gzipped tar from text files and base64-encoded
// files, keyed by workspace-relative path. It returns nil if both maps are
// empty.
func TarFromFiles(files, filesBase64 map[string]string) ([]byte, error) {
	contents := make(map[string][]byte, len(files)+len(filesBase64))
	add := func(p string, data []byte) error {
		c, err := CleanPath(p)
		if err != nil {
			return err
		}
		if _, dup := contents[c]; dup {
			return fmt.Errorf("path %q given more than once", p)
		}
		contents[c] = data
		return nil
	}
	for p, text := range files {
		if err := add(p, []byte(text)); err != nil {
			return nil, err
		}
	}
	for p, b64 := range filesBase64 {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("files_base64[%q]: %v", p, err)
		}
		if err := add(p, data); err != nil {
			return nil, err
		}
	}
	if len(contents) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	sort.Strings(names)
	// A file can't also be a directory.
	for _, name := range names {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if _, ok := contents[dir]; ok {
				return nil, fmt.Errorf("path %q is both a file and a directory", dir)
			}
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{}
	for _, name := range names {
		var parents []string
		for dir := path.Dir(name); dir != "." && !dirs[dir]; dir = path.Dir(dir) {
			parents = append(parents, dir)
			dirs[dir] = true
		}
		for i := len(parents) - 1; i >= 0; i-- {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: parents[i] + "/", Mode: 0o755}); err != nil {
				return nil, err
			}
		}
		data := contents[name]
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
