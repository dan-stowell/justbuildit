# Deploying justbuildit

## Host setup

On a fresh Ubuntu host with Docker installed:

    sudo deploy/setup.sh

It installs gVisor (`runsc`) from the gVisor apt repo, registers it with
Docker as runtime `runsc` with `--overlay2=root:memory` (writes to the
container's root filesystem live in sandbox memory and count against
`--memory`, so a build cannot fill host disk), creates the `justbuildit`
network (bridge `jbi0`, 172.30.0.0/16), installs the firewall, and pulls the
toolchain images. It is idempotent.

Firewall (`deploy/firewall.sh`, installed as
`/usr/local/sbin/justbuildit-firewall`): chain `JUSTBUILDIT-FWD` (the first
rule of `DOCKER-USER`, for traffic entering from `jbi0`) drops traffic to 0/8,
10/8, 100.64/10, 127/8, 169.254/16, 172.16/12, 192.168/16, 224/4 and 240/4.
Chain `JUSTBUILDIT-IN` (the first rule of `INPUT`) drops everything from
`jbi0` addressed to the host itself. Public internet is allowed. The chains
are replaced atomically with `iptables-restore --noflush`, so re-running it
never leaves a gap. `justbuildit-firewall.service` re-applies it at boot and
whenever Docker starts. The service checks the rules (`docker.CheckFirewall`)
at startup and refuses to start if they are missing, unless it is given
`--insecure-no-firewall` (never do that on a public host).

Images: refs must be on Docker Hub (explicit or implicit), ghcr.io, quay.io,
gcr.io, mcr.microsoft.com, public.ecr.aws or registry.k8s.io, with no ports
or IP addresses, because the Docker daemon pulls from the host network
outside the build firewall. Freshly pulled images over 8 GB are removed and
the build fails; after each pull, the least recently used non-catalog images
are removed while all images together exceed 40 GB. Each `VOLUME` an image
declares gets a 1 GB tmpfs (in sandbox memory) instead of an anonymous volume
on host disk. Builds run as `--user 0` and with `--log-driver=none`.

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

## Installing and updating the service

`deploy/justbuildit.service` runs `/usr/local/bin/justbuildit` as root (it
runs docker and reads iptables; docker group membership would be
root-equivalent anyway) on `:8000`, with data in `/var/lib/justbuildit`
(created by systemd). It starts after Docker and `justbuildit-firewall`,
restarts on failure, and gets only the environment set in the unit, so
nothing from an operator's shell (such as `BUILDBUDDY_API_KEY`) reaches it.
At startup it removes leftover build containers and wipes old build data
(the build index lives in memory).

First install, after `deploy/setup.sh` and fetching the binary as above:

    sudo install -m 755 out/cmd/justbuildit/justbuildit_/justbuildit /usr/local/bin/
    sudo install -m 644 deploy/justbuildit.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable --now justbuildit
    curl -s http://localhost:8000/llms.txt | head

To redeploy, build and download a new binary as above, then:

    sudo install -m 755 out/cmd/justbuildit/justbuildit_/justbuildit /usr/local/bin/
    sudo systemctl restart justbuildit

A restart cancels running builds and forgets all builds. Logs:
`journalctl -u justbuildit`.
