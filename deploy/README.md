# OpenAccessBridge deployment

OpenAccessBridge ships as **three supervised processes** on a **two-NIC** appliance
(buildplan §3.5, §4). This directory holds the two ways of running them — the
systemd units (`systemd/`) for a native install and the container packaging
(`docker/`) — plus the deployment-profile descriptors (`internal/deploy`).

Both are the same three binaries with the same flags, the same accounts and the
same privilege split; pick one per appliance. The native install is the
reference; the container packaging mirrors it unit for unit (D45).

## Processes (systemd)

| Unit | User | Role |
|---|---|---|
| `openaccessbridge-firewall.service` | root (CAP_NET_ADMIN) | nftables apply + forwarding/isolation verification — the only privileged component |
| `openaccessbridge-media.service` | `openaccessbridge` (+audio) | audio capture → qualifier → mapper → Opus → WebRTC |
| `openaccessbridge-core.service` | `openaccessbridge` (CAP_NET_BIND_SERVICE) | ACSP state machine, captions, web/admin, supervision |
| `openaccessbridge.target` | — | groups the three |

`core` **Wants** (not Requires) `media`, so a media crash never drops captions —
captions and audio are independent (§2). `core` **Requires** `firewall`, so the
network posture is verified before patrons are served.

### Install (low-cost/hardened tiers)

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin openaccessbridge
sudo install -m0755 bin/openaccessbridge-core bin/openaccessbridge-media bin/openaccessbridge-firewall /usr/local/bin/
sudo install -m0644 deploy/systemd/*.service deploy/systemd/*.target /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now openaccessbridge.target
```

First boot serves the setup wizard (§8.1): force admin-credential creation,
identify theater vs client NICs, set the cinema-server allowlist, pick a firewall
profile, run audio calibration, then run the readiness checks and exit setup.
Read "First boot" under Docker below before you start — the sequence and its
consequences are the same for a native install.

## Docker

One image, `ghcr.io/simpledcp/openaccessbridge`, carries all four binaries
(`deploy/docker/Dockerfile`: Ubuntu 24.04 — the base the audio path was verified
on — plus the GStreamer runtime the Opus encoder drives, `nft`, and the system
trust store core resolves `https` caption locators with; about 400 MB). Nothing
runs from it by default but `openaccessbridge-core`; the compose files and the
demo entrypoint choose what to run. Everything below assumes Docker Engine 25+
with Compose v2.20+ (`docker compose version`) and BuildKit (`docker buildx
version`) — on Ubuntu's `docker.io` package BuildKit's CLI is the separate
`docker-buildx` package, and `make docker-build` refuses to run without it.

### Build

```sh
make docker-build          # → ghcr.io/simpledcp/openaccessbridge:<git describe> and :latest
make docker-smoke          # build, then prove the image (what CI runs)
make docker-save           # → dist/openaccessbridge-<version>.tar, for an appliance (below)
```

Build through `make`, not `docker build` directly. The image gets the **same
version and build-number stamps** as the native binaries, and the Dockerfile
refuses to build without a positive build number: a binary with none refuses to
judge an update rather than guessing (D32). `make` also refuses to stamp from a
shallow clone, whose commit count is 1 — a known, wrong number. `docker compose
build` works too if `VERSION` and `BUILD_NUMBER` are in the environment.

### The appliance: `docker-compose.yml`

Three containers from the one image, mirroring the units one-to-one:

| Service | Runs as | Capabilities | Filesystem | Mirrors |
|---|---|---|---|---|
| `firewall` | `root:openaccessbridge` | `NET_ADMIN`, `NET_RAW` only; AppArmor and Docker's read-only `/proc/sys` lifted for this container so it can set the forwarding sysctls | read-only + the shared tmpfs | `openaccessbridge-firewall.service`, with `--allow-apply` |
| `media` | `openaccessbridge` (+`audio`) | none; Docker's mask over `/proc/asound` lifted so the wizard's device list works | read-only + the shared tmpfs; the host's live `/dev/snd` | `openaccessbridge-media.service` |
| `core` | `openaccessbridge` | `NET_BIND_SERVICE`, carried across `exec` by a file capability on the binary | read-only + the shared tmpfs + the config volume | `openaccessbridge-core.service` |

All three run on the **host network**, and not as a convenience. The helper
writes `ip_forward=0` and applies nftables to the host's own namespace, and the
ruleset it applies admits patron traffic only at the host's input hook, by NIC
name, with a forward chain that never accepts — so a bridge-networked container
behind Docker's NAT would receive nothing once the appliance is configured
(§9.1). `media` hands phones ICE candidates on the patron NIC rather than a
bridge address (§6.7), and `core` binds the patron NIC on :80 and the admin
surface on loopback exactly as its unit does (§8.4).

**The helper's privileges, precisely.** Docker mounts `/proc/sys` read-only and
its default AppArmor profile denies writes under `/proc/sys/net`; the helper must
write `ip_forward=0` and the IPv6 equivalent (guardrail #2). The compose file
therefore sets `apparmor=unconfined` and `systempaths=unconfined` on the helper
**only**, keeping every capability but `NET_ADMIN`/`NET_RAW` dropped, the default
seccomp profile, `no-new-privileges` and a read-only root. This was measured, not
assumed: with those two options the sysctls are writable and `nft` lists and
loads the host's tables; without either they are not. `privileged: true` would
also work and grants far more; it is not used.

**Why core needs a file capability.** The unit gives core
`AmbientCapabilities=CAP_NET_BIND_SERVICE`. A container runtime gives a non-root
user no ambient set, so `cap_add` alone is lost at `exec` and the `:80` bind
fails on the host network, where ports below 1024 are still privileged (measured:
`bind: permission denied`). The Dockerfile sets `cap_net_bind_service=+ep` on
the core binary; with `cap_add` keeping it in the bounding set, that is the whole
of what core can do that the service account cannot. One consequence: the core
binary in the image cannot be **executed** with that capability outside its
bounding set — the kernel refuses to run a file whose effective file capability
it cannot grant (`exec …: operation not permitted`) — so every way of running it,
the dev file included, keeps `cap_add: [NET_BIND_SERVICE]` beside `cap_drop: [ALL]`.

#### The appliance host

A dedicated two-NIC box that runs OpenAccessBridge and nothing else. Its Docker
daemon must run with `deploy/docker/daemon.json`:

```sh
sudo install -m0644 deploy/docker/daemon.json /etc/docker/daemon.json
sudo systemctl restart docker
ip link show docker0 2>&1 | grep -q 'does not exist' && echo "no bridge: good"
```

Why each key: dockerd creates a `docker0` **bridge** by default, and §9.3's
verification flags any bridge device as critical — and the setup wizard refuses
to complete over a critical warning, in every profile, so on a stock Docker host
the appliance could never leave setup (`bridge: none`). dockerd sets
`net.ipv4.ip_forward=1` at every start, which the helper's watchdog would flip
back every 30 s while the dashboard screams (`ip-forward: false`). Docker's own
`FORWARD`/NAT rules have no place beside a default-deny ruleset whose forward
chain never accepts (`iptables`/`ip6tables: false`). `live-restore` keeps the
three running across a daemon restart. With these settings the host cannot run
bridge-networked containers at all — nothing else may run on it, and no
`docker network create` either, since each one is another bridge. The three
appliance containers need none of it.

Give **both NICs static addresses** before starting: the ruleset admits no
DHCP, so a lease that expires after the ruleset is loaded is not renewed.

#### First boot

```sh
docker load -i openaccessbridge-<version>.tar                 # from `make docker-save` on a build host
docker compose -f deploy/docker/docker-compose.yml up -d      # or: make docker-up
docker compose -f deploy/docker/docker-compose.yml ps         # health of all three
docker compose -f deploy/docker/docker-compose.yml logs -f core
```

The admin surface is on `127.0.0.1:8443` **of the host**, as in the unit,
because production profiles refuse an admin listener reachable from the patron
network (§8.4). Reach it from the appliance's own console, or from the
installer's laptop over an SSH tunnel: `ssh -L 8443:127.0.0.1:8443 …` then
<http://127.0.0.1:8443/admin>. **Open the tunnel before the wizard's network and
server steps are saved**, and keep it open: from that point core has what it
needs to apply the ruleset, and the ruleset accepts no *new* inbound
connections on any port but the patron web ports — an SSH session that already
exists survives (established traffic is admitted), a new one does not, in any
profile. Lose it and the remaining path is the console. For the Booth
Diagnostic profile, which permits admin on the configured admin subnet, change
`--admin-bind` in the compose file to that NIC's address. The profile chosen in
the wizard takes effect when core next starts:
`docker compose -f deploy/docker/docker-compose.yml restart core` after setup
completes. Everything else runs as on the native install (§8.1).

#### What persists, and where

Only the SQLite config store, in the named volume `openaccessbridge_oab-state`
mounted at `/var/lib/openaccessbridge` (the unit's `StateDirectory=`). It is a
WAL database — up to three files — so back up the **directory**, with `core`
stopped, never a live copy:

```sh
C="docker compose -f deploy/docker/docker-compose.yml"
$C stop core
docker run --rm -v openaccessbridge_oab-state:/state:ro -v "$PWD":/out ubuntu:24.04 \
    tar czf /out/oab-config-$(date +%F).tgz -C /state .
$C start core
# restore: $C down; docker volume rm openaccessbridge_oab-state;
#          docker run --rm -v openaccessbridge_oab-state:/state -v "$PWD":/in ubuntu:24.04 tar xzf /in/oab-config-<date>.tgz -C /state
#          $C up -d
```

A bind mount works in place of the volume only if the directory is created first
as uid 1901 (the image's `openaccessbridge` account), mode 0700 — otherwise core
loops on "unable to open database file".

The runtime directory `/run/openaccessbridge` is a **tmpfs volume** shared by
the three containers — the unit's `RuntimeDirectory=` — so the loopback sockets
and any §9.4 debug capture live in memory only and vanish with `down`. `core`
refuses to start a capture anywhere it cannot prove is memory-backed, and it can
prove this one.

#### Audio hardware

`media` gets the host's **live** `/dev/snd` as a bind mount, with node access
granted by a cgroup rule for ALSA's character major — not a `devices:` entry,
which copies the device nodes once at create and drops the `by-id`/`by-path`
symlink directories the stable §6.1 device identity is read from. It also gets
`/proc/asound` unmasked, since Docker hides it from every unprivileged container
and `media` would otherwise report a booth with no sound subsystem (it says so
now, rather than listing no inputs — guardrail #7). This buys **device listing
for the §6.4 wizard**, no more: this build has no capture backend and refuses a
real device by name (B3), and Docker changes nothing about that. On a host with
no sound device Compose refuses to start `media` at all; remove the `/dev/snd`
bind there, and the process serves its synthetic tones (which the patron
surface declares as such).

#### Upgrades and rollback — offline

`docker pull` does not work on a configured appliance: the loaded ruleset's
output chain drops every new outbound flow except the cinema server's ACSP
port and, during a support window, one literal address (§9.1, §9.4). Images
travel as files:

```sh
make docker-save                                              # build host → dist/openaccessbridge-<version>.tar
docker load -i openaccessbridge-<version>.tar                 # on the appliance (console, or scp over the support tunnel)
docker compose -f deploy/docker/docker-compose.yml up -d      # recreates only the changed containers
```

The config volume is untouched and the store migrates its own schema on open
(§10.8). Rolling back is `up -d` with the previous image
(`OPENACCESSBRIDGE_IMAGE=…:<tag>`), **unless the upgrade bumped the config
schema**: the store refuses to open a database stamped newer than the build,
so a rollback across a schema bump means restoring the backup taken before the
upgrade. This is the whole update mechanism under Docker: the §9.4
signed-bundle path still verifies bundles and still cannot install them,
because a container image is not an A/B slot (B8, `ab_supported:false`).

#### Logs, time, health

Logs are JSON lines on stderr (`docker compose … logs -f --since 1h core`),
timestamped in UTC, rotated 10 MB × 5 per container, discarded when a container
is recreated, and by construction carry nothing sensitive (§9.4). Clock and NTP
are the host's, as natively (`timedatectl`); per-show tokens and support-window
deadlines depend on it. `docker compose ps` shows each container's health:
`core` answers `GET /healthz`; the helpers' checks are their sockets — which
proves each helper got past its listen step, not that it still answers. The
per-process liveness the operator should read is core's, on the dashboard (§3.5).

#### What the container packaging does not change

Every hardware and external-artifact blocker in `../BLOCKERS.md` is exactly as
open as before: no cinema-server wire contract (B1), no phones (B2), no capture
hardware behind the seam (B3), no A/B slots (B8). The image runs the same
synthetic audio source and the same in-process ACSP demo the unit runs, and
makes no claim the native install does not.

### A laptop or CI: `docker-compose.dev.yml`, or one container

```sh
make docker-build
docker compose -f deploy/docker/docker-compose.dev.yml up      # or: make docker-dev-up
# Patron page:  http://127.0.0.1:8080/accessibility
# Admin board:  http://127.0.0.1:8090/admin   (first boot: create credentials)
```

A separate Compose project (not an override of the appliance file — the two
differ in ways a merge cannot express). Same image, same three processes, same
account split and shared sticky tmpfs, so what it exercises is the container
packaging — but core and media on a bridge network with published ports, no
capabilities, and the helper in the dry-run it is on any dev host (B5). The
helper stays on the **host** network so the posture rows it measures describe
the laptop, as a native dev run's do: on a Docker host they read forwarding
ENABLED and a bridge PRESENT (`docker0`), which is true, and the ruleset row
"could not be confirmed" (no root). Ports are published on loopback only
(`OAB_PUBLISH_ADDR=0.0.0.0` opens them to the LAN — the wizard answers without a
credential until one is created). The admin surface is on `0.0.0.0` inside the
container, which only the non-production profiles allow (§8.4). WebRTC audio
plays only from the Docker host itself, since the ICE candidate is the
container's bridge address; captions work from anything that can reach the
patron port.

The same three processes in **one** container, for a first look:

```sh
docker run --rm -p 8080:8080 -p 8090:8090 ghcr.io/simpledcp/openaccessbridge openaccessbridge-demo
```

Started as root it drops `core` and `media` to the service account and keeps the
helper root, as the units do; started as any other user it runs all three as
that user, like the native dev recipe. One container is one supervised unit:
when the first process exits the rest are stopped and the container exits with
its status. `OAB_PATRON_BIND`, `OAB_ADMIN_BIND`, `OAB_DEMO` and `OAB_SYNTHETIC`
adjust it (defaults: `0.0.0.0:8080`, `0.0.0.0:8090`, `true`, `true`).

### The shared runtime directory (both deployments)

The three processes share one runtime directory for their loopback sockets, and
they run as two accounts: the helper as root with **exactly** `CAP_NET_ADMIN`
and `CAP_NET_RAW` — so no `CAP_DAC_OVERRIDE`, no `CAP_FOWNER` — and the other two
as `openaccessbridge`. The contract, enforced by `internal/ipc`:

- The directory is **group-writable for the shared group**, so root and the
  unprivileged pair can each create their socket in it regardless of which one
  is up first. Under Docker and in the demo it is provisioned **root-owned and
  sticky (1770)**, so the unprivileged pair can create and replace their own
  socket and the debug-capture sink but cannot remove the root helper's socket
  to put an impostor in its place. Under systemd, `RuntimeDirectory=` re-owns
  the directory to each unit's user as it starts, so there it is `0770`
  (`RuntimeDirectoryMode=`) owned by whichever unit started last; the sticky
  posture there would need only the helper's unit to own the directory, which
  is recorded as open (D45).
- The helper runs with that group as its **primary** group (`Group=` in the
  unit, `user: root:openaccessbridge` in Compose): its socket is handed to the
  group, and `core` is admitted by kernel-reported gid (§3.5).
- A process that finds the directory already there **leaves it as
  provisioned** — no chmod, no chown — after checking it is not a symlink, not
  world-writable, and owned by itself, root or the shared group. A process that
  creates the directory (a dev host, the OS temp dir) applies its own mode.
- A socket is **created at its final mode** (by umask around `bind`, never a
  chmod by path afterwards), its group set without following symlinks, and
  what sits at the path is then verified to be this process's own socket at
  that mode — a path that was swapped underneath the helper is refused, never
  re-moded.

Before D45 each process forced its own default mode on the shared directory and
the appliance depended on systemd re-owning it at every unit start; under any
other supervisor the helper died on its next restart with "operation not
permitted", and under systemd `core` could not create the debug-capture sink
after a helper restart.

## Deployment profiles (§9.2 / §12)

Defined in `internal/deploy` (`Builtin()`), selected in the setup wizard:

| Profile | Theater | Admin | SSH | Notes |
|---|---|---|---|---|
| **Lab** | broad | on setup net | local ok | dev only; warning banner; pcaps allowed |
| **Booth Diagnostic** | allowlist | admin subnet | off | lowest-risk first product; no access claim |
| **Patron-Facing** | allowlist | hidden from SSID | off | per-show token; purge at show end |
| **Low-Cost Replacement** | allowlist | separated | off | + mandatory calibration & signal health; **Phase 9 review required** before venue use |

**Loaner Device overlay** (§12.5): layers on Patron-Facing or Low-Cost Replacement
with a pre-trusted cert fleet, controlled browsers, supplied wired headphones, and
kiosk lock — a stronger access claim, but does not remove the Phase 9 review.

## Hard invariants enforced at the unit level

- **No content persistence (guardrail #5):** runtime state is on `tmpfs`
  (`RuntimeDirectory`, or the tmpfs volume under Docker); only the SQLite
  *config* store is writable (`/var/lib/openaccessbridge`, config only). **Core
  dumps are disabled** (`LimitCORE=0`, `ulimits: core: 0`) so a crash cannot leak
  caption text or audio to disk (§9.4).
- **No IP forwarding / default-deny (guardrails #2, #6):** the firewall helper
  sets `net.ipv4.ip_forward=0` + `net.ipv6.conf.all.forwarding=0` and applies a
  default-deny nftables ruleset with an empty `forward` chain, then continuously
  re-verifies by self-introspection (§9.3).
- **Privilege separation (§3.5):** only `firewall` is privileged; `core`/`media`
  run unprivileged with tight `ProtectSystem`, `NoNewPrivileges`, and restricted
  address families — or, under Docker, with every capability dropped, a
  read-only root filesystem and `no-new-privileges`.

## Patron Wi-Fi (§9.5)

Production profiles assume a **dedicated external AP** (5 GHz, Wi-Fi 6+),
band-steering controlled, client-isolation configured so phones can still reach
the appliance and mDNS resolves, DHCP scoped to capacity. Run the latency/soak
tests from representative **seats**, not the rack.

> Real hardware bring-up (cinema server, phones, audio jacks) is gated — see
> `../BLOCKERS.md`.
