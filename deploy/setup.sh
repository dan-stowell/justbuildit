#!/usr/bin/env bash
# Prepares a Docker host to run justbuildit builds. Idempotent; run as root:
#   sudo deploy/setup.sh
#
# - installs gVisor (runsc) from the official gVisor apt repo
# - registers the `runsc` Docker runtime with the container rootfs overlay
#   backed by memory, so writes inside a build count against --memory
#   instead of filling host disk
# - creates the `justbuildit` Docker network (bridge jbi0)
# - adds iptables rules so builds can reach the public internet but not
#   private ranges, link-local/metadata, or the host itself
# - pre-pulls the toolchain catalog images
#
# The iptables rules are not persisted; re-run this script after a reboot.
set -euo pipefail

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
)

if [[ $EUID -ne 0 ]]; then
  echo "run as root (sudo $0)" >&2
  exit 1
fi

log() { echo "== $*"; }

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
# Forwarded traffic from the build bridge to these destinations is dropped.
# Builds resolve DNS through public resolvers (the runner passes --dns), so
# nothing private needs to be reachable.
log "installing iptables rules"
BLOCKED=(
  0.0.0.0/8
  10.0.0.0/8
  100.64.0.0/10
  127.0.0.0/8
  169.254.0.0/16
  172.16.0.0/12
  192.168.0.0/16
  224.0.0.0/4
  240.0.0.0/4
)
iptables -N JUSTBUILDIT-FWD 2>/dev/null || iptables -F JUSTBUILDIT-FWD
for cidr in "${BLOCKED[@]}"; do
  iptables -A JUSTBUILDIT-FWD -d "$cidr" -j DROP
done
iptables -A JUSTBUILDIT-FWD -j RETURN
iptables -C DOCKER-USER -i "$BRIDGE" -j JUSTBUILDIT-FWD 2>/dev/null ||
  iptables -I DOCKER-USER -i "$BRIDGE" -j JUSTBUILDIT-FWD

# Traffic addressed to the host itself (any of its IPs, including the bridge
# gateway) goes through INPUT, not FORWARD; drop all of it.
iptables -N JUSTBUILDIT-IN 2>/dev/null || iptables -F JUSTBUILDIT-IN
iptables -A JUSTBUILDIT-IN -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A JUSTBUILDIT-IN -j DROP
iptables -C INPUT -i "$BRIDGE" -j JUSTBUILDIT-IN 2>/dev/null ||
  iptables -I INPUT -i "$BRIDGE" -j JUSTBUILDIT-IN

# --- Images -----------------------------------------------------------------
log "pulling catalog images"
for img in "${IMAGES[@]}"; do
  docker pull -q "$img"
done

log "done"
