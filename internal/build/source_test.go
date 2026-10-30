package build_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"testing"

	"github.com/dan-stowell/justbuildit/internal/build"
)

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		"a.go":       "a.go",
		"dir/b.go":   "dir/b.go",
		"dir//c":     "dir/c",
		"dir/./d":    "dir/d",
		"x/y/z.txt":  "x/y/z.txt",
		".hidden/rc": ".hidden/rc",
	} {
		got, err := build.CleanPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "/abs", "/", "..", "../x", "a/../../x", "a/..", `a\b`, "a\x00b"} {
		if got, err := build.CleanPath(in); err == nil {
			t.Errorf("CleanPath(%q) = %q, want error", in, got)
		}
	}
}

func TestTarFromFilesRejectsBadPaths(t *testing.T) {
	for _, files := range []map[string]string{
		{"/etc/passwd": "x"},
		{"../escape": "x"},
		{"a/../../escape": "x"},
		{"a": "x", "a/b": "y"}, // a is both file and dir
		{"a": "x", "./a": "y"}, // duplicate after cleaning
	} {
		if _, err := build.TarFromFiles(files, nil); err == nil {
			t.Errorf("TarFromFiles(%v): want error", files)
		}
	}
	if _, err := build.TarFromFiles(nil, map[string]string{"../x": base64.StdEncoding.EncodeToString([]byte("x"))}); err == nil {
		t.Errorf("TarFromFiles with bad base64 path: want error")
	}
}

func TestTarFromFiles(t *testing.T) {
	if data, err := build.TarFromFiles(nil, nil); err != nil || data != nil {
		t.Fatalf("empty: got %d bytes, %v", len(data), err)
	}
	data, err := build.TarFromFiles(
		map[string]string{"main.go": "package main\n", "pkg/sub/x.go": "package sub\n"},
		map[string]string{"bin/blob": base64.StdEncoding.EncodeToString([]byte{0, 1, 2})},
	)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	var order []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, h.Name)
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	want := map[string]string{
		"bin/":         "",
		"bin/blob":     "\x00\x01\x02",
		"main.go":      "package main\n",
		"pkg/":         "",
		"pkg/sub/":     "",
		"pkg/sub/x.go": "package sub\n",
	}
	if len(got) != len(want) {
		t.Fatalf("entries %v, want %v", order, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("entry %q = %q, want %q", k, got[k], v)
		}
	}
}
