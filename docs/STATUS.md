# Status (2026-10-30)

## Done
- Service on :8000 (systemd `justbuildit`, binary built on RBE, see deploy/README.md):
  REST `POST /v1/builds` (JSON files, raw tar.gz), polling, logs, artifacts;
  MCP at `/mcp` (build, get_build, read_artifact); agent guide at `/llms.txt`.
- Builds run under gVisor (runsc, rootfs overlay in memory) with an egress
  firewall (no private/link-local/host), resource caps, attacker-safe artifact
  stream, image size/registry limits, admission limits on uploads/waiters.
- Three subagent customers built Go, Rust, C, Python, TS, cobra, ripgrep and
  zlib through it successfully.

## Next (in priority order)
1. Image pulls (security): pulls run in dockerd outside the sandbox. A layer's
   `urls` makes the host send GETs to private addresses (SSRF), and arbitrary
   images can fill disk. Plan: allow only Docker Hub official images, check
   free disk before pull, prune every image not in use by a running build.
2. Firewall in one place: render and apply iptables from Go at startup,
   validate the `justbuildit` network/bridge, block SMTP and non-DNS UDP;
   make Docker's /etc/hosts and /etc/hostname mounts read-only.
3. Disk budget for retained builds (logs + artifacts); align runner and
   service artifact caps (500MB); extend the service safety-net deadline by
   the pull timeout so slow pulls report timed_out, not error.
4. Agent feedback: queue past the per-client running cap instead of 429;
   derive URLs and client IP from the exe.dev proxy (X-Forwarded-*);
   never-null response fields + warnings; `**` artifact globs; multipart
   upload (JSON request + tarball); list/cancel builds; smaller MCP results;
   quieter pip/npm logs; guide tips (go.sum, root, exec bit).
5. Issues: #3 dependency caching, #4 Docker Hub auth/mirror, #5 non-root.

Branches `runner-hardening-2` and `agent-feedback-1` were cut for 1-3 and 4
but have no commits yet.
