# Deploying justbuildit

## Host setup

On a fresh Ubuntu host with Docker installed:

    sudo deploy/setup.sh

It installs gVisor (`runsc`) from the gVisor apt repo, registers it with
Docker as runtime `runsc` with `--overlay2=root:memory` (writes to the
container's root filesystem live in sandbox memory and count against
`--memory`, so a build cannot fill host disk), creates the `justbuildit`
network (bridge `jbi0`, 172.30.0.0/16), installs iptables rules, and pulls the
toolchain images. It is idempotent. The iptables rules are not persisted, so
re-run it after a reboot.

Firewall: chain `JUSTBUILDIT-FWD` (jumped to from `DOCKER-USER` for traffic
entering from `jbi0`) drops traffic to 0/8, 10/8, 100.64/10, 127/8,
169.254/16, 172.16/12, 192.168/16, 224/4 and 240/4. Chain `JUSTBUILDIT-IN`
(jumped to from `INPUT`) drops everything from `jbi0` addressed to the host
itself. Public internet is allowed.

DNS: Docker's embedded resolver (127.0.0.11) is not reachable from gVisor's
network stack, and this host's resolver is 169.254.169.254, which is blocked.
The runner therefore bind-mounts its own `/etc/resolv.conf` pointing at public
resolvers (1.1.1.1, 8.8.8.8 by default; see `docker.Config.DNS`).

## Checking the sandbox

`cmd/runnercheck` runs real builds through `internal/docker` against this
host's Docker and gVisor, including hostile ones (symlinked artifacts, `../`
tar entries, writing 10 GB, reaching metadata and host ports, timeouts) and
one build per toolchain image:

    runnercheck        # all checks
    runnercheck -v -run network

## Getting a binary built on RBE onto the host

Nothing is built on the host. Build with `bb remote build`, pinning the inner
Bazel invocation ID, then download that invocation's outputs:

    ID=$(cat /proc/sys/kernel/random/uuid)
    bb remote build //cmd/justbuildit --invocation_id=$ID \
      --remote_executor=grpcs://buildbuddy.buildbuddy.io \
      --remote_cache=grpcs://buildbuddy.buildbuddy.io \
      --bes_backend=grpcs://buildbuddy.buildbuddy.io \
      --bes_results_url=https://buildbuddy.buildbuddy.io/invocation/ \
      --remote_download_minimal --jobs=100
    bb download artifacts $ID --output_directory=out
    install -m 755 out/cmd/justbuildit/justbuildit_/justbuildit /usr/local/bin/

`bb download artifacts` takes the Bazel invocation (the one printed as
"Invocation ID:" in the output), not the outer `bb remote` run, which has no
artifacts. The binary is static (pure Go), so it runs as is.
