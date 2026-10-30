#!/usr/bin/env bash
# Installs the iptables rules that keep justbuildit builds (traffic entering
# from bridge jbi0) off private ranges, link-local/metadata and the host,
# while allowing the public internet. Idempotent and safe to re-run while
# builds are running: the JUSTBUILDIT chains are flushed and refilled in one
# atomic iptables-restore, and the jumps to them are only ever added, never
# removed first. deploy/setup.sh installs this as
# /usr/local/sbin/justbuildit-firewall, run by justbuildit-firewall.service
# at boot and whenever Docker (re)starts.
#
# internal/docker.CheckFirewall checks for exactly these rules; keep the two
# in sync.
set -euo pipefail

BRIDGE=jbi0
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

# Forwarded traffic from the build bridge to BLOCKED is dropped. Traffic
# addressed to the host itself (any of its IPs, including the bridge
# gateway) goes through INPUT, not FORWARD; all of it is dropped.
{
  echo '*filter'
  echo ':JUSTBUILDIT-FWD - [0:0]'
  echo ':JUSTBUILDIT-IN - [0:0]'
  for cidr in "${BLOCKED[@]}"; do
    echo "-A JUSTBUILDIT-FWD -d $cidr -j DROP"
  done
  echo '-A JUSTBUILDIT-FWD -j RETURN'
  echo '-A JUSTBUILDIT-IN -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN'
  echo '-A JUSTBUILDIT-IN -j DROP'
  echo 'COMMIT'
} | iptables-restore --noflush

# ensure_first CHAIN RULE...: make RULE the first rule of CHAIN, inserting
# the new copy before deleting any old ones so there is no unfiltered gap.
ensure_first() {
  local chain=$1
  shift
  local rule="-A $chain $*"
  if [[ "$(iptables -S "$chain" | grep -m1 '^-A ' || true)" != "$rule" ]]; then
    iptables -I "$chain" 1 "$@"
  fi
  local n
  while n=$(iptables -S "$chain" | grep '^-A ' | grep -nxF -- "$rule" | sed -n '2s/:.*//p'); [[ -n $n ]]; do
    iptables -D "$chain" "$n"
  done
}

# Docker creates DOCKER-USER (and jumps to it first from FORWARD) when it
# starts; create it if Docker has not yet.
iptables -N DOCKER-USER 2>/dev/null || true
ensure_first DOCKER-USER -i "$BRIDGE" -j JUSTBUILDIT-FWD
ensure_first INPUT -i "$BRIDGE" -j JUSTBUILDIT-IN
