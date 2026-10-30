package build_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
)

// writeTar writes a gzipped tar with the given entries, mimicking what a
// runner produces with tar czf (including ./ prefixes and odd entries).
func writeTar(t *testing.T, path string, entries []*tar.Header, contents map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		data := contents[h.Name]
		h.Size = int64(len(data))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(data))
		}
	}
	tw.Close()
	gz.Close()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

var sampleEntries = func() []*tar.Header {
	return []*tar.Header{
		{Name: "./out/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./out/hello", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "out/notes.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "out/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
	}
}

var sampleContents = map[string]string{
	"./out/hello":   "ELF...",
	"out/notes.txt": "hello notes\n",
	"../evil":       "nope",
}

func TestListArtifacts(t *testing.T) {
	p := filepath.Join(t.TempDir(), build.ArtifactsFile)
	if arts, err := build.ListArtifacts(p); err != nil || arts != nil {
		t.Fatalf("missing tarball: %v, %v", arts, err)
	}
	writeTar(t, p, sampleEntries(), sampleContents)
	arts, err := build.ListArtifacts(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []build.Artifact{{Path: "out/hello", Size: 6}, {Path: "out/notes.txt", Size: 12}}
	if len(arts) != len(want) {
		t.Fatalf("artifacts = %+v, want %+v", arts, want)
	}
	for i := range want {
		if arts[i] != want[i] {
			t.Errorf("artifact %d = %+v, want %+v", i, arts[i], want[i])
		}
	}
}

func TestOpenArtifact(t *testing.T) {
	p := filepath.Join(t.TempDir(), build.ArtifactsFile)
	writeTar(t, p, sampleEntries(), sampleContents)

	for name, want := range map[string]string{"out/notes.txt": "hello notes\n", "out/hello": "ELF...", "./out/hello": "ELF..."} {
		rc, size, err := build.OpenArtifact(p, name)
		if err != nil {
			t.Fatalf("OpenArtifact(%q): %v", name, err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		if string(data) != want || size != int64(len(want)) {
			t.Errorf("OpenArtifact(%q) = %q (size %d), want %q", name, data, size, want)
		}
	}
	for _, name := range []string{"out/missing", "out/link", "out", "../evil", "/etc/passwd", ""} {
		if rc, _, err := build.OpenArtifact(p, name); !errors.Is(err, build.ErrNotFound) {
			if rc != nil {
				rc.Close()
			}
			t.Errorf("OpenArtifact(%q): err = %v, want ErrNotFound", name, err)
		}
	}
	if _, _, err := build.OpenArtifact(filepath.Join(t.TempDir(), "none.tar.gz"), "x"); !errors.Is(err, build.ErrNotFound) {
		t.Errorf("missing tarball: err = %v, want ErrNotFound", err)
	}
}

func TestServiceArtifacts(t *testing.T) {
	svc, err := build.New(build.Config{DataDir: t.TempDir(), Run: func(ctx context.Context, spec build.Spec, log io.Writer) (build.Outcome, error) {
		writeTar(t, filepath.Join(spec.Dir, build.ArtifactsFile), sampleEntries(), sampleContents)
		return build.Outcome{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	res, err := svc.Build(context.Background(), build.Request{Command: "x", Artifacts: []string{"out/*"}}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("artifacts = %+v", res.Artifacts)
	}
	rc, _, err := svc.OpenArtifact(res.ID, "out/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello notes\n" {
		t.Errorf("artifact = %q", data)
	}
	if _, _, err := svc.OpenArtifact(res.ID, "out/missing"); !errors.Is(err, build.ErrNotFound) {
		t.Errorf("missing artifact: err = %v, want ErrNotFound", err)
	}
}
