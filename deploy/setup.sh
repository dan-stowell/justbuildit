#!/usr/bin/env bash
# Prepares a Docker host to run justbuildit builds. Idempotent; run as root:
#   sudo deploy/setup.sh
#
# - installs gVisor (runsc) from the official gVisor apt repo
# - registers the `runsc` Docker runtime with the container rootfs overlay
#   backed by memory, so writes inside a build count against --memory
#   instead of filling host disk
# - creates the `justbuildit` Docker network (bridge jbi0)
# - installs deploy/firewall.sh (iptables rules so builds can reach the
#   public internet but not private ranges, link-local/metadata, or the host
#   itself) with a systemd unit that re-applies it at boot and whenever
#   Docker starts
# - pre-pulls the toolchain catalog images
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

NETWORK=justbuildit
BRIDGE=jbi0
SUBNET=172.30.0.0/16
IMAGES=(
  golang:1.25
  node:22
  python:3.13
  rust:1
  gcc:14
  maven:3-eclipse-temurin-21
  buildpack-deps:bookworm  # build.DefaultImage
)

if [[ $EUID -ne 0 ]]; then
  echo "run as root (sudo $0)" >&2
  exit 1
fi

log() { echo "== $*"; }

if ! command -v jq >/dev/null; then
  log "installing jq"
  apt-get update -qq
  apt-get install -y -qq jq
fi

# --- gVisor -----------------------------------------------------------------
if ! command -v runsc >/dev/null || ! dpkg -s runsc >/dev/null 2>&1; then
  log "installing runsc"
  apt-get update -qq
  apt-get install -y -qq apt-transport-https ca-certificates curl gnupg
  curl -fsSL https://gvisor.dev/archive.key |
    gpg --dearmor --yes -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
    >/etc/apt/sources.list.d/gvisor.list
  apt-get update -qq
  apt-get install -y -qq runsc
fi
runsc --version | head -1

# --- Docker runtime ---------------------------------------------------------
# --overlay2=root:memory: the container's root filesystem gets a writable
# overlay layer kept in sandbox memory (charged to the container's memory
# cgroup), rather than the default root:self, which backs it with a file on
# host disk.
log "configuring docker runtime runsc"
daemon=/etc/docker/daemon.json
mkdir -p /etc/docker
[[ -s $daemon ]] || echo '{}' >"$daemon"
want=$(jq --arg path "$(command -v runsc)" \
  '.runtimes.runsc = {path: $path, runtimeArgs: ["--overlay2=root:memory"]}' "$daemon")
if [[ "$want" != "$(jq . "$daemon")" ]]; then
  echo "$want" >"$daemon"
  systemctl restart docker
fi

# --- Network ----------------------------------------------------------------
if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
  log "creating network $NETWORK"
  docker network create --driver bridge --subnet "$SUBNET" \
    -o com.docker.network.bridge.name="$BRIDGE" \
    -o com.docker.network.bridge.enable_icc=false \
    "$NETWORK"
fi

# --- Firewall ---------------------------------------------------------------
log "installing firewall"
install -m 755 "$HERE/firewall.sh" /usr/local/sbin/justbuildit-firewall
cat >/etc/systemd/system/justbuildit-firewall.service <<'UNIT'
[Unit]
Description=justbuildit build network firewall
After=docker.service
PartOf=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/justbuildit-firewall

[Install]
WantedBy=docker.service multi-user.target
UNIT
systemctl daemon-reload
systemctl enable justbuildit-firewall.service
systemctl restart justbuildit-firewall.service

# --- Images -----------------------------------------------------------------
log "pulling catalog images"
for img in "${IMAGES[@]}"; do
  docker pull -q "$img"
done

log "done"
