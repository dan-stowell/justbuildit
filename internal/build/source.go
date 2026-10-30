package build

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
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

// TarFromFiles writes to w a gzipped tar of text files and base64-encoded
// files, keyed by workspace-relative path. It writes only regular-file
// entries; extraction creates their parent directories. Problems with the
// files themselves are reported, as *InvalidError or ErrTooLarge, before
// anything is written.
func TarFromFiles(w io.Writer, files, filesBase64 map[string]string) error {
	if n := len(files) + len(filesBase64); n > MaxSourceFiles {
		return invalid("%d files given; at most %d are allowed (send a tarball body for larger workspaces)", n, MaxSourceFiles)
	}
	contents := make(map[string]string, len(files)+len(filesBase64))
	var total int
	add := func(p string, data string) error {
		if len(p) > MaxPathLen {
			return invalid("path %.40q...: longer than %d characters", p, MaxPathLen)
		}
		c, err := CleanPath(p)
		if err != nil {
			return invalid("%v", err)
		}
		if strings.Count(c, "/") >= MaxPathDepth {
			return invalid("path %q: more than %d levels deep", p, MaxPathDepth)
		}
		if _, dup := contents[c]; dup {
			return invalid("path %q given more than once", p)
		}
		if total += len(data); total > MaxSourceBytes {
			return ErrTooLarge
		}
		contents[c] = data
		return nil
	}
	for p, text := range files {
		if err := add(p, text); err != nil {
			return err
		}
	}
	for p, b64 := range filesBase64 {
		if total+base64.StdEncoding.DecodedLen(len(b64)) > MaxSourceBytes+2 {
			return ErrTooLarge
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return invalid("files_base64[%q]: %v", p, err)
		}
		if err := add(p, string(data)); err != nil {
			return err
		}
	}

	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	sort.Strings(names)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data := contents[name]
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
