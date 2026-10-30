// Package fakerun provides a build.RunFunc for tests that needs no docker.
package fakerun

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dan-stowell/justbuildit/internal/build"
)

// New returns a RunFunc that writes "running COMMAND" to the log, copies
// every workspace file under out/ into the artifacts tarball, and exits 0.
// If the command is "block", it first waits until release is closed.
func New(release <-chan struct{}) build.RunFunc {
	return func(ctx context.Context, spec build.Spec, log io.Writer) (build.Outcome, error) {
		fmt.Fprintf(log, "running %s\n", spec.Command)
		if spec.Command == "block" {
			select {
			case <-release:
			case <-ctx.Done():
				return build.Outcome{}, ctx.Err()
			}
		}
		if spec.SourceFile == "" {
			return build.Outcome{}, nil
		}
		return build.Outcome{}, copyOut(spec)
	}
}

func copyOut(spec build.Spec) error {
	src, err := os.Open(spec.SourceFile)
	if err != nil {
		return err
	}
	defer src.Close()
	gr, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gr)
	out, err := os.Create(filepath.Join(spec.Dir, build.ArtifactsFile))
	if err != nil {
		return err
	}
	defer out.Close()
	gw := gzip.NewWriter(out)
	tw := tar.NewWriter(gw)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && strings.HasPrefix(h.Name, "out/") {
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}
