// Package docker runs builds in sandboxed Docker containers using the docker
// CLI.
//
// The host never reads or writes the build's files. The gzipped source tar is
// streamed to the container on stdin and extracted there by the image's own
// sh and tar; afterwards the same wrapper packs the matching artifacts into
// one gzipped tar and writes it to the container's stdout, which the host
// copies to Dir/artifacts.tar.gz. The build's own stdout and stderr both go
// to the container's stderr, which is the build log.
//
// Everything the container writes is attacker-controlled (the build can
// replace sh and tar), so the artifact stream is size-, entry- and
// type-checked before it is kept, and reading it never outlives the build's
// timeout.
package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
)

// Label marks containers started by this package.
const Label = "justbuildit"

// Config holds host-wide settings for running builds.
type Config struct {
	Docker           string        // docker CLI binary; default "docker"
	Runtime          string        // OCI runtime, e.g. "runsc"; empty uses Docker's default
	Network          string        // Docker network; empty uses Docker's default
	Memory           string        // --memory, e.g. "4g"; also used as --memory-swap
	CPUs             string        // --cpus, e.g. "2"
	PidsLimit        int           // --pids-limit; 0 means unset
	DNS              []string      // nameservers written to the container's /etc/resolv.conf
	PullTimeout      time.Duration // default 5m
	MaxArtifactBytes int64         // cap on artifacts.tar.gz; default 1 GiB

	// MaxArtifactUnpackedBytes caps the decompressed size of the artifact
	// tar stream (headers included); default 2 GiB.
	MaxArtifactUnpackedBytes int64
	// MaxArtifactEntries caps the number of entries in it; default 100000.
	MaxArtifactEntries int

	// VolumeSize is the tmpfs-size of the tmpfs mounted over each VOLUME
	// the image declares, so writes there stay in sandbox memory instead
	// of an anonymous volume on host disk; default "1g".
	VolumeSize string

	// MaxImageBytes caps the disk usage of a freshly pulled image; bigger
	// images are removed and the build fails. Default 8 GB.
	MaxImageBytes int64
	// ImageBudgetBytes is the total disk usage of images above which the
	// least recently used images not in KeepImages are removed after a
	// pull; 0 disables pruning.
	ImageBudgetBytes int64
	// KeepImages are never pruned.
	KeepImages []string

	// Bridge is the host interface of Network that CheckFirewall expects
	// the JUSTBUILDIT iptables chains to filter.
	Bridge string
	// IPTables is the iptables binary CheckFirewall runs; default "iptables".
	IPTables string
}

// DefaultConfig is the production configuration; it expects deploy/setup.sh
// to have been run on the host.
func DefaultConfig() Config {
	keep := []string{build.DefaultImage}
	for _, tc := range build.Toolchains() {
		keep = append(keep, tc.Image)
	}
	return Config{
		Runtime:          "runsc",
		Network:          "justbuildit",
		Memory:           "4g",
		CPUs:             "2",
		PidsLimit:        1024,
		DNS:              []string{"1.1.1.1", "8.8.8.8"},
		ImageBudgetBytes: 40e9,
		KeepImages:       keep,
		Bridge:           "jbi0",
	}
}

func (c Config) withDefaults() Config {
	if c.Docker == "" {
		c.Docker = "docker"
	}
	if c.PullTimeout == 0 {
		c.PullTimeout = 5 * time.Minute
	}
	if c.MaxArtifactBytes == 0 {
		c.MaxArtifactBytes = 1 << 30
	}
	if c.MaxArtifactUnpackedBytes == 0 {
		c.MaxArtifactUnpackedBytes = 2 << 30
	}
	if c.MaxArtifactEntries == 0 {
		c.MaxArtifactEntries = 100000
	}
	if c.VolumeSize == "" {
		c.VolumeSize = "1g"
	}
	if c.MaxImageBytes == 0 {
		c.MaxImageBytes = 8e9
	}
	if c.IPTables == "" {
		c.IPTables = "iptables"
	}
	return c
}

// Deadlines for docker CLI calls that must not hang a build forever.
const (
	killTimeout   = 10 * time.Second // one `docker kill`
	killGrace     = 30 * time.Second // after this, kill the docker CLI too
	removeTimeout = 30 * time.Second // `docker rm -f`
)

// wrapper runs inside the container as `sh -c wrapper justbuildit CMD GLOB...`
// with the working directory set to /work. Its stdout is the artifacts
// channel; everything else goes to stderr.
const wrapper = `exec 3>&1 1>&2
if [ "${JUSTBUILDIT_SOURCE:-}" = 1 ]; then
  tar -xzf - || { echo "justbuildit: extracting source failed"; exit 1; }
fi
sh -c "$1" </dev/null 3>&-
status=$?
shift
n=$#
IFS=
for g in "$@"; do
  for f in $g; do
    if [ -e "$f" ] || [ -L "$f" ]; then
      case $f in /*) ;; *) f=./$f ;; esac
      set -- "$@" "$f"
    fi
  done
done
shift $n
if [ $# -gt 0 ]; then
  tar -czf - "$@" >&3 || echo "justbuildit: packing artifacts failed"
fi
exit $status
`

// Run runs spec in a fresh container, streaming the build log to log. It
// writes spec.Dir/artifacts.tar.gz only if some artifact matched. A non-nil
// error means the build could not be run, not that it failed.
func Run(ctx context.Context, cfg Config, spec build.Spec, log io.Writer) (build.Outcome, error) {
	cfg = cfg.withDefaults()
	if err := checkImage(spec.Image); err != nil {
		return build.Outcome{}, err
	}
	img, err := ensureImage(ctx, cfg, spec.Image, log)
	if err != nil {
		return build.Outcome{}, err
	}
	volumes, err := volumeMounts(img, cfg.VolumeSize)
	if err != nil {
		return build.Outcome{}, err
	}

	resolv := filepath.Join(spec.Dir, "resolv.conf")
	if err := os.WriteFile(resolv, resolvConf(cfg.DNS), 0o644); err != nil {
		return build.Outcome{}, err
	}
	defer os.Remove(resolv)

	name := "justbuildit-" + spec.ID
	cmd := exec.Command(cfg.Docker, runArgs(cfg, spec, name, resolv, volumes)...)
	cmd.Stdin = bytes.NewReader(spec.Source)
	cmd.Stderr = log
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return build.Outcome{}, err
	}
	if err := cmd.Start(); err != nil {
		return build.Outcome{}, fmt.Errorf("starting docker: %w", err)
	}
	defer removeContainer(cfg, name)

	// Reading artifacts stops when the build times out or ctx ends, even if
	// the container keeps its stdout open or the stream is a bomb.
	actx, stopArtifacts := context.WithCancel(context.Background())
	defer stopArtifacts()
	lim := limits{packed: cfg.MaxArtifactBytes, unpacked: cfg.MaxArtifactUnpackedBytes, entries: cfg.MaxArtifactEntries}
	done := make(chan runResult, 1)
	go func() {
		// Drain stdout before Wait, which closes it.
		note, aerr := saveArtifacts(actx, stdout, spec.Dir, lim)
		done <- runResult{note, aerr, cmd.Wait()}
	}()

	var timeout <-chan time.Time
	if spec.Timeout > 0 {
		t := time.NewTimer(spec.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	var res runResult
	timedOut := false
	select {
	case res = <-done:
	case <-timeout:
		timedOut = true
		stopArtifacts()
		res = stopUntilDone(cfg, name, cmd.Process, done)
	case <-ctx.Done():
		stopArtifacts()
		stopUntilDone(cfg, name, cmd.Process, done)
		return build.Outcome{}, ctx.Err()
	}
	if res.artifacts != nil {
		return build.Outcome{}, fmt.Errorf("saving artifacts: %w", res.artifacts)
	}
	if timedOut {
		fmt.Fprintf(log, "\njustbuildit: build timed out after %s\n", spec.Timeout)
		return build.Outcome{ExitCode: -1, TimedOut: true}, nil
	}
	if res.note != "" {
		fmt.Fprintf(log, "justbuildit: %s\n", res.note)
	}

	werr := res.wait
	var exitErr *exec.ExitError
	switch {
	case werr == nil:
		return build.Outcome{ExitCode: 0}, nil
	case errors.As(werr, &exitErr):
		code := exitErr.ExitCode()
		if code == 125 && !containerExited125(cfg, name) {
			// docker run itself failed (bad flag, daemon error, ...).
			return build.Outcome{}, fmt.Errorf("docker run failed (exit 125); see log")
		}
		if code == 137 {
			fmt.Fprintf(log, "\njustbuildit: build was killed (exit 137), most likely for exceeding the %s memory limit; files written inside the container count toward it\n", cfg.Memory)
		}
		return build.Outcome{ExitCode: code}, nil
	default:
		return build.Outcome{}, werr
	}
}

func runArgs(cfg Config, spec build.Spec, name, resolv string, volumes []string) []string {
	args := []string{"run", "-i",
		"--name", name,
		"--label", Label + "=1",
		"--log-driver", "none",
		"--user", "0:0",
		"--entrypoint", "sh",
		"--workdir", "/work",
		"--mount", "type=bind,source=" + resolv + ",target=/etc/resolv.conf,readonly",
	}
	for _, v := range volumes {
		args = append(args, "--mount", v)
	}
	if len(spec.Source) > 0 {
		args = append(args, "--env", "JUSTBUILDIT_SOURCE=1")
	}
	if cfg.Runtime != "" {
		args = append(args, "--runtime", cfg.Runtime)
	}
	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}
	if cfg.Memory != "" {
		args = append(args, "--memory", cfg.Memory, "--memory-swap", cfg.Memory)
	}
	if cfg.CPUs != "" {
		args = append(args, "--cpus", cfg.CPUs)
	}
	if cfg.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(cfg.PidsLimit))
	}
	args = append(args, spec.Image, "-c", wrapper, "justbuildit", spec.Command)
	return append(args, spec.Artifacts...)
}

func resolvConf(servers []string) []byte {
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	return []byte(b.String())
}

// volumeMounts returns a --mount value putting a size-bounded tmpfs over
// each VOLUME the image declares; otherwise Docker would back them with
// anonymous volumes on host disk, bypassing the in-memory rootfs overlay.
func volumeMounts(img imageInfo, size string) ([]string, error) {
	var out []string
	for _, v := range img.Volumes {
		if !strings.HasPrefix(v, "/") || path.Clean(v) != v || strings.ContainsAny(v, ",\"'=\r\n") {
			return nil, fmt.Errorf("image declares an unsupported VOLUME %q", v)
		}
		out = append(out, "type=tmpfs,destination="+v+",tmpfs-size="+size)
	}
	return out, nil
}

// Registries images may be pulled from. A ref without a registry is a
// Docker Hub image.
var allowedRegistries = []string{
	"docker.io", "gcr.io", "ghcr.io", "mcr.microsoft.com", "public.ecr.aws", "quay.io", "registry.k8s.io",
}

var (
	pathComponentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRE           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRE        = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// checkImage accepts only plain refs to images on allowedRegistries, so the
// Docker daemon (which pulls from the host's network, outside the build
// firewall) is never pointed at private addresses, ports or localhost.
func checkImage(ref string) error {
	bad := func(why string) error { return fmt.Errorf("invalid image %q: %s", ref, why) }
	if ref == "" || len(ref) > 255 {
		return bad("empty or too long")
	}
	name, digest, hasDigest := strings.Cut(ref, "@")
	if hasDigest && !digestRE.MatchString(digest) {
		return bad("bad digest")
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		if !tagRE.MatchString(name[i+1:]) {
			return bad("bad tag")
		}
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		reg := parts[0]
		ok := false
		for _, r := range allowedRegistries {
			ok = ok || reg == r
		}
		if !ok {
			return bad("registry " + reg + " is not allowed (allowed: " + strings.Join(allowedRegistries, ", ") + ")")
		}
		parts = parts[1:]
	}
	for _, p := range parts {
		if !pathComponentRE.MatchString(p) {
			return bad("bad repository name")
		}
	}
	return nil
}

type imageInfo struct {
	ID       string
	RepoTags []string
	Volumes  []string
}

// inspectImage returns ID, tags and VOLUMEs of a local image.
func inspectImage(ctx context.Context, cfg Config, ref string) (imageInfo, error) {
	out, err := exec.CommandContext(ctx, cfg.Docker, "image", "inspect", ref).Output()
	if err != nil {
		return imageInfo{}, err
	}
	var imgs []struct {
		ID       string `json:"Id"`
		RepoTags []string
		Config   struct{ Volumes map[string]struct{} }
	}
	if err := json.Unmarshal(out, &imgs); err != nil || len(imgs) != 1 {
		return imageInfo{}, fmt.Errorf("inspecting image %s: unexpected output", ref)
	}
	info := imageInfo{ID: imgs[0].ID, RepoTags: imgs[0].RepoTags}
	for v := range imgs[0].Config.Volumes {
		info.Volumes = append(info.Volumes, v)
	}
	sort.Strings(info.Volumes)
	return info, nil
}

// lastUsed records when this process last ran each image ID, for pruning.
// imagesMu also serializes pruning against looking up images, so an image
// is never pruned between being found and being marked used.
var (
	imagesMu sync.Mutex
	lastUsed = map[string]time.Time{}
)

// recentUse protects images used this recently (longer than any build)
// from pruning.
const recentUse = 2 * time.Hour

func ensureImage(ctx context.Context, cfg Config, ref string, log io.Writer) (imageInfo, error) {
	imagesMu.Lock()
	info, err := inspectImage(ctx, cfg, ref)
	if err == nil {
		lastUsed[info.ID] = time.Now()
	}
	imagesMu.Unlock()
	if err == nil {
		return info, nil
	}

	fmt.Fprintf(log, "justbuildit: pulling %s\n", ref)
	if err := pull(ctx, cfg, ref); err != nil {
		fmt.Fprintf(log, "justbuildit: %v\n", err)
		return imageInfo{}, err
	}

	imagesMu.Lock()
	defer imagesMu.Unlock()
	info, err = inspectImage(ctx, cfg, ref)
	if err != nil {
		return imageInfo{}, fmt.Errorf("inspecting %s after pull: %w", ref, err)
	}
	sizes, err := imageSizes(ctx, cfg)
	if err != nil {
		return imageInfo{}, err
	}
	if size := sizes[info.ID]; size > cfg.MaxImageBytes {
		removeImage(cfg, info)
		err := fmt.Errorf("image %s is too big (%d MB; limit %d MB)", ref, size/1e6, cfg.MaxImageBytes/1e6)
		fmt.Fprintf(log, "justbuildit: %v\n", err)
		return imageInfo{}, err
	}
	lastUsed[info.ID] = time.Now()
	pruneImages(ctx, cfg, sizes)
	return info, nil
}

// pull pulls ref, reporting failure as one short line rather than the
// daemon's own diagnostics.
func pull(ctx context.Context, cfg Config, ref string) error {
	pctx, cancel := context.WithTimeout(ctx, cfg.PullTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(pctx, cfg.Docker, "pull", "-q", ref)
	cmd.Stdout = io.Discard
	cmd.Stderr = &limitedBuffer{buf: &out, n: 64 << 10}
	if err := cmd.Run(); err != nil {
		if pctx.Err() != nil {
			return fmt.Errorf("pulling %s: timed out after %s", ref, cfg.PullTimeout)
		}
		msg := strings.ToLower(out.String())
		why := "registry error"
		switch {
		case strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist"):
			why = "image not found"
		case strings.Contains(msg, "denied") || strings.Contains(msg, "unauthorized"):
			why = "access denied (only public images can be used)"
		}
		return fmt.Errorf("pulling %s failed: %s", ref, why)
	}
	return nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	n   int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// imageSizes maps each local image ID to its disk usage as `docker image
// ls` reports it (which, unlike inspect's Size, includes unpacked layers
// with the containerd image store).
func imageSizes(ctx context.Context, cfg Config) (map[string]int64, error) {
	out, err := exec.CommandContext(ctx, cfg.Docker, "image", "ls", "--no-trunc", "--format", "{{.ID}} {{.Size}}").Output()
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	sizes := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, size, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, err := parseSize(size)
		if err != nil {
			return nil, fmt.Errorf("listing images: %w", err)
		}
		sizes[id] = n
	}
	return sizes, nil
}

var sizeRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kMGTP]?)B$`)

// parseSize parses Docker's human-readable (decimal) sizes like "1.26GB".
func parseSize(s string) (int64, error) {
	m := sizeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("unexpected size %q", s)
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15}[m[2]]
	return int64(f * mult), nil
}

// pruneImages removes the least recently used images not in KeepImages and
// not used within recentUse until the total is within ImageBudgetBytes.
// Call with imagesMu held.
func pruneImages(ctx context.Context, cfg Config, sizes map[string]int64) {
	if cfg.ImageBudgetBytes <= 0 {
		return
	}
	var total int64
	for _, s := range sizes {
		total += s
	}
	if total <= cfg.ImageBudgetBytes {
		return
	}
	keep := map[string]bool{}
	for _, ref := range cfg.KeepImages {
		if info, err := inspectImage(ctx, cfg, ref); err == nil {
			keep[info.ID] = true
		}
	}
	var ids []string
	for id := range sizes {
		if !keep[id] && time.Since(lastUsed[id]) > recentUse {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return lastUsed[ids[i]].Before(lastUsed[ids[j]]) })
	for _, id := range ids {
		if total <= cfg.ImageBudgetBytes {
			return
		}
		info, err := inspectImage(ctx, cfg, id)
		if err != nil {
			continue
		}
		if removeImage(cfg, info) == nil {
			total -= sizes[id]
			delete(lastUsed, id)
		}
	}
}

// removeImage removes all of an image's tags and so the image. It fails if
// a container still uses it.
func removeImage(cfg Config, info imageInfo) error {
	refs := info.RepoTags
	if len(refs) == 0 {
		refs = []string{info.ID}
	}
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, cfg.Docker, append([]string{"image", "rm"}, refs...)...).Run()
}

type runResult struct {
	note            string // message about discarded artifacts, for the log
	artifacts, wait error
}

// stopUntilDone kills the container, retrying in case it was still being
// created, until docker run exits. If the container cannot be killed within
// killGrace it kills the docker CLI itself, which ends the streams.
func stopUntilDone(cfg Config, name string, cli *os.Process, done <-chan runResult) runResult {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	grace := time.After(killGrace)
	for {
		// The container may not exist yet or may already be gone.
		ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
		_ = exec.CommandContext(ctx, cfg.Docker, "kill", name).Run()
		cancel()
		select {
		case r := <-done:
			return r
		case <-grace:
			cli.Kill()
			grace = nil
		case <-tick.C:
		}
	}
}

// removeContainer force-removes the build's container, whatever state it
// is in.
func removeContainer(cfg Config, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	_ = exec.CommandContext(ctx, cfg.Docker, "rm", "-f", name).Run()
}

// containerExited125 reports whether the container itself ran and exited
// with status 125, as opposed to docker run failing with 125.
func containerExited125(cfg Config, name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, cfg.Docker, "container", "inspect", "--format",
		"{{.State.ExitCode}}|{{.State.StartedAt}}|{{.State.Error}}", name).Output()
	if err != nil {
		return false
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	return len(f) == 3 && f[0] == "125" && !strings.HasPrefix(f[1], "0001-") && f[2] == ""
}

type limits struct {
	packed, unpacked int64
	entries          int
}

// ctxReader fails reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// saveArtifacts copies the artifact stream to dir/artifacts.tar.gz, keeping
// it only if it is non-empty and passes checkTarGz; otherwise note says why
// it was dropped. It drains r so the container never blocks on its stdout,
// until ctx is done. err is set only for host-side failures.
func saveArtifacts(ctx context.Context, r io.Reader, dir string, lim limits) (note string, err error) {
	r = ctxReader{ctx, r}
	defer io.Copy(io.Discard, r)
	f, err := os.CreateTemp(dir, ".artifacts-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	n, err := io.Copy(f, io.LimitReader(r, lim.packed+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case ctx.Err() != nil:
		return "artifacts discarded: build stopped", nil
	case err != nil:
		return "", err
	case n == 0:
		return "", nil
	case n > lim.packed:
		return fmt.Sprintf("artifacts exceed %d bytes; discarded", lim.packed), nil
	}
	if err := checkTarGz(ctx, tmp, lim); err != nil {
		return fmt.Sprintf("artifacts discarded: %v", err), nil
	}
	return "", os.Rename(tmp, filepath.Join(dir, build.ArtifactsFile))
}

// checkTarGz checks that path is a gzipped tar of at most lim.entries
// regular files, directories, symlinks and hard links, decompressing to at
// most lim.unpacked bytes. Sparse files are rejected: they expand to
// arbitrary amounts of zeros when read.
func checkTarGz(ctx context.Context, path string, lim limits) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(ctxReader{ctx, f})
	if err != nil {
		return fmt.Errorf("not a valid tar.gz (%v)", err)
	}
	// Both the decompressed stream and the entry contents read from it are
	// capped: they differ for sparse entries, which checkEntry also rejects.
	stream := &io.LimitedReader{R: zr, N: lim.unpacked + 1}
	tr := tar.NewReader(stream)
	var content int64
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if err == nil && entries >= lim.entries {
			return fmt.Errorf("more than %d entries", lim.entries)
		}
		if err == nil {
			if err := checkEntry(h); err != nil {
				return err
			}
		}
		if err == nil {
			var n int64
			n, err = io.Copy(io.Discard, io.LimitReader(ctxReader{ctx, tr}, lim.unpacked-content+1))
			content += n
		}
		switch {
		case stream.N <= 0 || content > lim.unpacked:
			return fmt.Errorf("unpack to more than %d bytes", lim.unpacked)
		case ctx.Err() != nil:
			return ctx.Err()
		case err == io.EOF:
			return nil
		case err != nil:
			return fmt.Errorf("not a valid tar.gz (%v)", err)
		}
	}
}

func checkEntry(h *tar.Header) error {
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return fmt.Errorf("%q is a sparse file", h.Name)
		}
	}
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
		return nil
	case tar.TypeGNUSparse:
		return fmt.Errorf("%q is a sparse file", h.Name)
	}
	return fmt.Errorf("%q has unsupported type %q", h.Name, h.Typeflag)
}

// Cleanup force-removes containers left over from a previous run of the
// service. Call it once at startup, before any builds start.
func Cleanup(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults()
	out, err := exec.CommandContext(ctx, cfg.Docker, "ps", "-aq", "--filter", "label="+Label).Output()
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	rm := exec.CommandContext(ctx, cfg.Docker, append([]string{"rm", "-f"}, ids...)...)
	if out, err := rm.CombinedOutput(); err != nil {
		return fmt.Errorf("removing containers: %v: %s", err, out)
	}
	return nil
}

// blockedRanges are the destinations deploy/firewall.sh drops for traffic
// from the build bridge; keep the two in sync.
var blockedRanges = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
}

// CheckFirewall verifies that the iptables rules from deploy/firewall.sh are
// in place for cfg.Bridge: the JUSTBUILDIT chains are jumped to first from
// DOCKER-USER and INPUT and hold exactly the expected rules. The service should
// refuse to start if it fails. It needs root.
func CheckFirewall(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults()
	if cfg.Bridge == "" {
		return errors.New("firewall check: no bridge configured")
	}
	rules := func(chain string) ([]string, error) {
		out, err := exec.CommandContext(ctx, cfg.IPTables, "-S", chain).Output()
		if err != nil {
			return nil, fmt.Errorf("firewall check: listing chain %s: %v (run deploy/setup.sh)", chain, err)
		}
		var rs []string
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "-A ") {
				rs = append(rs, strings.TrimSpace(l))
			}
		}
		return rs, nil
	}
	for chain, jump := range map[string]string{"DOCKER-USER": "JUSTBUILDIT-FWD", "INPUT": "JUSTBUILDIT-IN"} {
		rs, err := rules(chain)
		if err != nil {
			return err
		}
		want := fmt.Sprintf("-A %s -i %s -j %s", chain, cfg.Bridge, jump)
		if len(rs) == 0 || rs[0] != want {
			return fmt.Errorf("firewall check: first rule of %s is not %q (run deploy/setup.sh)", chain, want)
		}
	}
	// The JUSTBUILDIT chains must hold exactly the rules firewall.sh writes.
	want := map[string][]string{"JUSTBUILDIT-IN": {
		"-A JUSTBUILDIT-IN -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-A JUSTBUILDIT-IN -j DROP",
	}}
	for _, cidr := range blockedRanges {
		want["JUSTBUILDIT-FWD"] = append(want["JUSTBUILDIT-FWD"], "-A JUSTBUILDIT-FWD -d "+cidr+" -j DROP")
	}
	want["JUSTBUILDIT-FWD"] = append(want["JUSTBUILDIT-FWD"], "-A JUSTBUILDIT-FWD -j RETURN")
	for chain, must := range want {
		rs, err := rules(chain)
		if err != nil {
			return err
		}
		if strings.Join(rs, "\n") != strings.Join(must, "\n") {
			return fmt.Errorf("firewall check: chain %s does not hold the expected rules (run deploy/setup.sh)", chain)
		}
	}
	return nil
}
