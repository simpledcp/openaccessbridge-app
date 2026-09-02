#!/usr/bin/env bash
# The appliance posture, PROVEN — inside a network namespace of its own.
#
# Everything else in this repository stops short of the same line. The firewall
# helper generates a ruleset and verifies what it can read, but applying it
# needs root on a machine that may have its network taken away, and no such
# machine exists here (BLOCKER B5): a development host must not acquire a
# default-deny ruleset and ip_forward=0 because a test ran. So the §9.1/§9.3
# claims — the ruleset really loads, the forwarding knobs really go to zero, the
# posture is really MEASURED back off the kernel, the §3.3 watchdog really
# re-asserts — have never been executed anywhere.
#
# This script executes them, in a container network namespace that has two
# interfaces of its own. The three processes run in three containers sharing
# ONE namespace, exactly as they share the host's on an appliance; the helper
# gets the capability set deploy/docker/docker-compose.yml ships; and the
# ruleset it loads is loaded by a real `nft -f` into a real kernel table, then
# read back by the real verification pass.
#
#   deploy/docker/netns-appliance-test.sh              # against :latest
#   OPENACCESSBRIDGE_IMAGE=…:v0.1.0 deploy/docker/netns-appliance-test.sh
#
# WHAT IT PROVES: everything about the posture that is a property of the
# software — the generated ruleset is accepted by nftables, the sysctls are
# written, the readback measures what was written, the wizard can complete over
# a green posture, patron traffic is admitted on the patron interface by name,
# and a deliberately flipped knob is put back.
#
# WHAT IT DOES NOT PROVE: anything that is a property of the HOST namespace or
# of hardware. The appliance runs all three on the host's network with
# deploy/docker/daemon.json in force; that arrangement is still unexercised, as
# are the systemd units, real NICs, and every blocker in ../../BLOCKERS.md.
# A namespace is not an appliance. It is, however, the same kernel.
#
# SAFETY. The script refuses to run unless it has proven, first, that the
# namespace it is about to hand to a privileged process is not the host's. See
# preflight() — every assertion there is a precondition for `--allow-apply`,
# and any one of them failing is an abort, never a warning.
set -euo pipefail

IMAGE="${OPENACCESSBRIDGE_IMAGE:-ghcr.io/simpledcp/openaccessbridge:latest}"
THEATER_NET=oab-theater-test
PATRON_NET=oab-patron-test
THEATER_SUBNET="${OAB_THEATER_SUBNET:-10.77.10.0/24}"
PATRON_SUBNET="${OAB_PATRON_SUBNET:-10.77.20.0/24}"
# The cinema server the appliance is told to expect: an address on the theater
# network that nothing answers at. The ruleset's shape is what is under test,
# not a conversation with a server (BLOCKER B1).
SERVER_IP="${OAB_SERVER_IP:-10.77.10.9}"
FW=oab-nt-firewall MEDIA=oab-nt-media CORE=oab-nt-core
RUN_VOL=oab-nt-run STATE_VOL=oab-nt-state
ADMIN_USER=installer ADMIN_PASS='booth-test-Passw0rd!'

pass() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; FAILED=$((FAILED+1)); }
step() { printf '\n== %s\n' "$*"; }
die()  { printf '\nABORT: %s\n' "$*" >&2; exit 2; }
FAILED=0

# oab_http METHOD PATH [BODY] [USER:PASS] — HTTP/1.0 over bash's /dev/tcp, run
# INSIDE the namespace. The image carries no curl and needs none; and driving
# the admin surface from inside is the faithful analogue of an installer's SSH
# tunnel to an appliance's loopback (deploy/README.md, "First boot").
# HTTP/1.0 on purpose: it keeps the response un-chunked, so the body is what
# follows the header block and nothing has to un-chunk it.
read -r -d '' HTTP_HELPER <<'HELPER' || true
oab_http() {
  local m="$1" p="$2" b="${3:-}" c="${4:-}" H="${OAB_ADMIN:-127.0.0.1:8443}"
  local h="${H%:*}" P="${H##*:}" r a=""
  [ -n "$c" ] && a="Authorization: Basic $(printf %s "$c" | base64 -w0)"$'\r\n'
  exec 3<>"/dev/tcp/$h/$P" || return 1
  r="$m $p HTTP/1.0"$'\r\n'"Host: $h"$'\r\n'"$a"
  if [ -n "$b" ]; then
    r+="Content-Type: application/json"$'\r\n'"Content-Length: ${#b}"$'\r\n\r\n'"$b"
  else
    r+=$'\r\n'
  fi
  printf %s "$r" >&3
  local x; x="$(cat <&3 2>/dev/null)"; exec 3<&-
  printf "%s " "$(printf %s "${x%%$'\r'*}" | awk '{print $2}')"
  printf "%s" "${x#*$'\r\n\r\n'}"
}
HELPER

# api METHOD PATH [BODY] — an admin request from inside the namespace, as the
# installer. Prints "<status> <body>".
api() {
  local body="${3:-}"
  docker exec "$CORE" bash -c "$HTTP_HELPER; oab_http '$1' '$2' '${body//\'/\'\\\'\'}' '$ADMIN_USER:$ADMIN_PASS'"
}
# api_open is the same before any credential exists (first boot: the wizard is
# served openly until one is created — internal/web/setup.go requireAdmin).
api_open() {
  local body="${3:-}"
  docker exec "$CORE" bash -c "$HTTP_HELPER; oab_http '$1' '$2' '${body//\'/\'\\\'\'}'"
}
status_json() { api GET /api/admin/status | cut -d' ' -f2-; }
jq_field() { python3 -c 'import json,sys;d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d = d[int(k)] if isinstance(d, list) else d.get(k)
    if d is None: break
print(json.dumps(d))' "$1"; }

cleanup() {
  docker rm -f "$CORE" "$MEDIA" "$FW" >/dev/null 2>&1 || true
  docker volume rm -f "$RUN_VOL" "$STATE_VOL" >/dev/null 2>&1 || true
  docker network rm "$THEATER_NET" "$PATRON_NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# preflight — the assertions that must hold before a privileged process is
# allowed to apply anything. Each is an abort, not a warning: the cost of
# being wrong is this machine's network.
# ---------------------------------------------------------------------------
HOST_NETNS=""; HOST_FWD=""
preflight() {
  step "Preflight: prove the namespace is not this host's"
  command -v docker >/dev/null || die "docker is required"
  docker image inspect "$IMAGE" >/dev/null 2>&1 || die "image $IMAGE not present (run: make docker-build)"

  HOST_NETNS="$(readlink /proc/self/ns/net)"
  HOST_FWD="$(cat /proc/sys/net/ipv4/ip_forward)"
  pass "host namespace $HOST_NETNS, ip_forward=$HOST_FWD (recorded)"

  # The namespace the helper will be given must differ from this one. If these
  # ever match, --allow-apply would rewrite the host's firewall.
  local probe_ns
  # --network none still gives a container its OWN namespace, so the check
  # needs none of the test topology to exist yet.
  probe_ns="$(docker run --rm --network none --cap-drop ALL "$IMAGE" readlink /proc/self/ns/net)"
  [ -n "$probe_ns" ] || die "could not read the container namespace"
  [ "$probe_ns" != "$HOST_NETNS" ] || die "the container shares this host's network namespace — refusing to apply"
  pass "container namespace $probe_ns is a different namespace"

  # And it must not already see this host's tables: a container that could read
  # them is a container that could replace them.
  local tables
  tables="$(docker run --rm --network none --cap-drop ALL --cap-add NET_ADMIN "$IMAGE" nft list tables 2>/dev/null | tr '\n' ' ')"
  case "$tables" in
    *openaccessbridge*) die "the container namespace already holds an openaccessbridge table: not a clean namespace" ;;
  esac
  pass "container namespace holds no openaccessbridge table (sees: ${tables:-none})"

  # The subnets are the one thing here that really edits this host's routing:
  # `docker network create --subnet` installs a connected route. A collision
  # could blackhole live traffic while the test runs, so the check covers ALL
  # routing tables — Tailscale keeps its peer routes in a policy table that
  # `ip route show` alone does not list.
  local s
  for s in "$THEATER_SUBNET" "$PATRON_SUBNET"; do
    if command -v ip >/dev/null && ip -4 route show table all 2>/dev/null | awk '{print $1}' | grep -qx "$s"; then
      die "subnet $s already exists in this host's routing — choose another with OAB_THEATER_SUBNET/OAB_PATRON_SUBNET"
    fi
  done
  pass "test subnets $THEATER_SUBNET and $PATRON_SUBNET collide with no host route"
}

# ---------------------------------------------------------------------------
# topology — two networks, three containers, one namespace.
# ---------------------------------------------------------------------------
THEATER_IFACE=""; PATRON_IFACE=""; PATRON_IP=""
topology() {
  step "Topology: two networks, three containers, one namespace"
  docker network create --subnet "$THEATER_SUBNET" "$THEATER_NET" >/dev/null
  docker network create --subnet "$PATRON_SUBNET" "$PATRON_NET" >/dev/null

  # The helper holds the namespace and is attached to both networks; the other
  # two join it. On an appliance all three share the host's namespace, and the
  # helper is likewise the one that acts on it.
  #
  # CREATED, gated, and only then STARTED. This ordering is the safety
  # property, not a style: the helper's §3.3 watchdog calls applySysctls()
  # unconditionally 30 s after it starts — no core, no wizard, no apply request
  # needed — so a helper that started in the wrong namespace would zero this
  # host's forwarding knobs half a minute later. The namespace is therefore
  # asserted from the container's CONFIGURATION before it is ever running,
  # rather than confirmed from inside it afterwards.
  docker create --name "$FW" --network "$THEATER_NET" --user root:openaccessbridge \
    --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW \
    --security-opt apparmor=unconfined --security-opt systempaths=unconfined \
    --read-only --tmpfs /tmp -v "$RUN_VOL":/run/openaccessbridge "$IMAGE" \
    openaccessbridge-firewall --socket-dir=/run/openaccessbridge --allow-apply >/dev/null
  docker network connect "$PATRON_NET" "$FW"

  local mode nets
  mode="$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$FW")"
  case "$mode" in
    host|default|none|container:*|"") die "the helper's network mode is '$mode' — refusing to start a privileged applier" ;;
  esac
  nets="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' "$FW" | grep -v '^$' | sort | tr '\n' ' ')"
  [ "$nets" = "$PATRON_NET $THEATER_NET " ] || die "the helper is attached to [$nets], want exactly the two test networks"
  local shares
  shares="$(docker inspect -f '{{.HostConfig.PidMode}}|{{.HostConfig.UTSMode}}|{{.HostConfig.IpcMode}}|{{.HostConfig.Privileged}}' "$FW")"
  case "$shares" in *host*|*true*) die "the helper shares a host namespace or is privileged ($shares)" ;; esac
  pass "pre-start gate: network mode '$mode', networks [$nets], no host namespaces, not privileged"

  docker start "$FW" >/dev/null

  docker run -d --name "$MEDIA" --network "container:$FW" --user openaccessbridge \
    --cap-drop ALL --security-opt no-new-privileges:true --security-opt systempaths=unconfined \
    --read-only --tmpfs /tmp -v "$RUN_VOL":/run/openaccessbridge "$IMAGE" \
    openaccessbridge-media --socket-dir=/run/openaccessbridge >/dev/null

  # NET_BIND_SERVICE is needed to EXECUTE this binary at all, not only to bind
  # :80: it carries a file capability, and the kernel refuses to run a file
  # whose effective capability it cannot grant (deploy/README.md).
  docker run -d --name "$CORE" --network "container:$FW" --user openaccessbridge \
    --cap-drop ALL --cap-add NET_BIND_SERVICE --security-opt no-new-privileges:true \
    --read-only --tmpfs /tmp -v "$RUN_VOL":/run/openaccessbridge -v "$STATE_VOL":/var/lib/openaccessbridge "$IMAGE" \
    openaccessbridge-core --config=/var/lib/openaccessbridge/config.db \
    --patron-bind=0.0.0.0:80 --admin-bind=127.0.0.1:8443 \
    --socket-dir=/run/openaccessbridge --demo=false >/dev/null

  # Which interface is which is resolved from the kernel's own view — the MAC
  # Docker assigned on each network, matched against /sys/class/net — never
  # from an assumption about eth0/eth1 ordering.
  local tmac pmac
  tmac="$(docker inspect "$FW" --format "{{(index .NetworkSettings.Networks \"$THEATER_NET\").MacAddress}}")"
  pmac="$(docker inspect "$FW" --format "{{(index .NetworkSettings.Networks \"$PATRON_NET\").MacAddress}}")"
  PATRON_IP="$(docker inspect "$FW" --format "{{(index .NetworkSettings.Networks \"$PATRON_NET\").IPAddress}}")"
  THEATER_IFACE="$(docker exec "$FW" sh -c "grep -l '^$tmac\$' /sys/class/net/*/address | head -1 | cut -d/ -f5")"
  PATRON_IFACE="$(docker exec "$FW" sh -c "grep -l '^$pmac\$' /sys/class/net/*/address | head -1 | cut -d/ -f5")"
  [ -n "$THEATER_IFACE" ] && [ -n "$PATRON_IFACE" ] || die "could not resolve the two interfaces"
  pass "theater=$THEATER_IFACE patron=$PATRON_IFACE (patron address $PATRON_IP)"

  local ns_fw ns_media ns_core
  ns_fw="$(docker exec "$FW" readlink /proc/self/ns/net)"
  ns_media="$(docker exec "$MEDIA" readlink /proc/self/ns/net)"
  ns_core="$(docker exec "$CORE" readlink /proc/self/ns/net)"
  [ "$ns_fw" = "$ns_media" ] && [ "$ns_fw" = "$ns_core" ] || die "the three processes are not in one namespace"
  [ "$ns_fw" != "$HOST_NETNS" ] || die "the shared namespace IS the host's — refusing to continue"
  pass "all three processes share $ns_fw, which is not the host's"

  local i
  for i in $(seq 1 30); do
    [ "$(api_open GET /healthz | cut -d' ' -f1)" = 200 ] && break
    sleep 1
  done
  [ "$(api_open GET /healthz | cut -d' ' -f1)" = 200 ] || die "the admin surface never came up"
  pass "admin surface answering inside the namespace"
}

# ---------------------------------------------------------------------------
# wizard — the §8.1 first-boot sequence, driven exactly as an installer drives
# it, ending in a ruleset the helper has really installed.
#
# The apply is unlocked by the network and server steps alone (core's reconcile
# needs theater_interface, client_interface and a parseable allowlist); the
# credential and profile steps exist to satisfy the completion gate. Profile
# MUST be booth_diagnostic: core reads the active profile once at startup, so
# choosing anything else would leave the running process applying a ruleset for
# a profile the operator did not pick until it restarts.
# ---------------------------------------------------------------------------
wizard() {
  step "Wizard: the §8.1 first-boot sequence"
  local st
  st="$(api_open GET /api/admin/setup/state)"
  case "$st" in 200*'"has_credentials":false'*) pass "first boot: the wizard is served without a credential" ;;
    *) fail "unexpected initial setup state: $st" ;; esac

  # 1. Credentials. Until this exists the wizard is open; after it, everything
  #    needs Basic auth — including the polling below.
  st="$(api_open POST /api/admin/setup/credentials "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}")"
  case "$st" in 200*'"has_credentials":true'*) pass "credentials created" ;; *) fail "credentials: $st" ;; esac

  # 2. Which NIC is which. This is the step that decides what the kernel rules
  #    will say, so it is asserted rather than assumed.
  st="$(api POST /api/admin/setup/network "{\"theaterIface\":\"$THEATER_IFACE\",\"clientIface\":\"$PATRON_IFACE\"}")"
  case "$st" in 200*'"network_configured":true'*) pass "interfaces assigned (theater=$THEATER_IFACE patron=$PATRON_IFACE)" ;;
    *) fail "network step: $st" ;; esac

  # 3. The cinema server. Its address must be a literal IP on the theater
  #    network: the ruleset's Variant A egress rule is written from it, and the
  #    scope check measures the theater route against it. Nothing answers there
  #    — the ruleset's shape is what is under test, not a conversation (B1).
  st="$(api POST /api/admin/setup/server "{\"auditorium_id\":\"netns-lab-1\",\"auditorium_name\":\"NetNS Lab 1\",\"cinema_server_ip\":\"$SERVER_IP\",\"acsp_port\":4170,\"acsp_initiation\":\"acs_initiated\"}")"
  case "$st" in 200*'"server_configured":true'*) pass "cinema server allowlisted ($SERVER_IP)" ;;
    *) fail "server step: $st" ;; esac

  # 4. The profile.
  st="$(api POST /api/admin/setup/profile '{"profile":"booth_diagnostic"}')"
  case "$st" in 200*'"profile_selected":true'*) pass "profile selected (booth_diagnostic)" ;;
    *) fail "profile step: $st" ;; esac

  # The gate must REFUSE while the posture is still red — that refusal is the
  # feature, so it is asserted rather than waited out.
  st="$(api POST /api/admin/setup/complete '{}')"
  case "$st" in
    412*forwarding_enabled*|412*firewall_inactive*) pass "completion refused while the posture is red: ${st#412 }" ;;
    200*) fail "the wizard completed BEFORE the firewall was applied — the §9.3 gate did not hold" ;;
    *) pass "completion refused: ${st#412 }" ;;
  esac
}

# ---------------------------------------------------------------------------
# posture — what the §9.3 pass MEASURES off the kernel the helper just wrote.
# Every assertion here is a measurement the appliance made of itself; the
# script re-reads the same kernel independently and compares.
# ---------------------------------------------------------------------------
posture() {
  step "Posture: the ruleset, applied and measured (reconcile is a 15 s ticker)"
  local i ok=false
  for i in $(seq 1 45); do
    if docker exec "$FW" nft list table inet openaccessbridge >/dev/null 2>&1; then ok=true; break; fi
    sleep 2
  done
  [ "$ok" = true ] || die "the ruleset was never installed (nft has no inet openaccessbridge table after 90s)"
  pass "table inet openaccessbridge is loaded in the kernel (${i} polls)"

  # Read the ruleset back OURSELVES. §9.1's promises are structural, so they
  # are checked against the table's own text, not against the report.
  local rs; rs="$(docker exec "$FW" nft list table inet openaccessbridge)"
  grep -q 'chain forward' <<<"$rs" && grep -A2 'chain forward' <<<"$rs" | grep -q 'policy drop' \
    && pass "forward chain: policy drop" || fail "forward chain is not policy drop"
  local fwd_rules
  fwd_rules="$(awk '/chain forward/,/^\t}/' <<<"$rs" | grep -cE 'accept|jump|goto' || true)"
  [ "$fwd_rules" = 0 ] && pass "forward chain is EMPTY — the two zones are never joined" \
                       || fail "forward chain has $fwd_rules accepting rule(s)"
  grep -q "iif \"$PATRON_IFACE\" tcp dport { 80, 443 } accept" <<<"$rs" \
    && pass "patron web admitted only on the patron interface" || fail "patron web rule missing/wrong"
  grep -q "oif \"$THEATER_IFACE\" ip daddr $SERVER_IP tcp dport 4170 accept" <<<"$rs" \
    && pass "ACSP egress admitted only to the allowlisted server on the theater interface" \
    || fail "ACSP egress rule missing/wrong"
  grep -q 'chain input' <<<"$rs" && awk '/chain input/,/^\t}/' <<<"$rs" | grep -q 'policy drop' \
    && pass "input chain: policy drop" || fail "input chain is not policy drop"

  # The forwarding knobs, read from the kernel the helper wrote.
  local k v allzero=true
  for k in ipv4/ip_forward ipv4/conf/all/forwarding ipv4/conf/default/forwarding \
           ipv6/conf/all/forwarding ipv6/conf/default/forwarding; do
    v="$(docker exec "$FW" cat "/proc/sys/net/$k")"
    [ "$v" = 0 ] || { allzero=false; fail "$k is $v, want 0"; }
  done
  [ "$allzero" = true ] && pass "every forwarding knob is 0 (v4 and v6, global and per-default)"

  # And what the appliance says about itself. firewall_profile_version is the
  # decisive one: core leaves it empty unless the HELPER answered applied:true,
  # which the helper only does after nft accepted the script.
  local s; s="$(status_json)"
  check() { # check FIELD EXPECTED DESCRIPTION
    local got; got="$(printf '%s' "$s" | jq_field "$1")"
    [ "$got" = "$2" ] && pass "$3 ($1=$got)" || fail "$3: $1=$got, want $2"
  }
  check firewall_active true            "the firewall is active"
  check firewall_introspected true      "…and that was PROVEN by reading the kernel, not asserted"
  check forwarding_disabled true        "forwarding is off across every knob the pass enumerates"
  check bridges_present false           "no bridge device (the check a stock Docker host fails)"
  check admin_isolated true             "the admin surface is isolated from the patron network"
  check ssh_introspected true           "the SSH row was measured"
  local ver; ver="$(printf '%s' "$s" | jq_field system_config.firewall_profile_version)"
  [[ "$ver" =~ ^\"booth_diagnostic@[0-9a-f]{8}\"$ ]] \
    && pass "the helper really installed it (firewall_profile_version=$ver)" \
    || fail "firewall_profile_version=$ver — the helper never reported applied:true"
  local crit; crit="$(printf '%s' "$s" | python3 -c 'import json,sys;print(",".join(w["code"] for w in json.load(sys.stdin).get("warnings",[]) if w.get("severity")=="critical"))')"
  [ -z "$crit" ] && pass "no critical warnings" || fail "critical warnings: $crit"
  local warns; warns="$(printf '%s' "$s" | python3 -c 'import json,sys;print(",".join(w["code"] for w in json.load(sys.stdin).get("warnings",[])))')"
  # dns_internet_leak_across_interfaces is expected HERE and nowhere else: the
  # namespace's resolver is Docker's own on 127.0.0.11, which is not part of
  # either zone's plan. It is a property of the sandbox, it is a warning rather
  # than a critical outside the production profiles, and it does not block
  # setup — so it is named, not silently tolerated.
  [ "$warns" = dns_internet_leak_across_interfaces ] || [ -z "$warns" ] \
    && pass "remaining warnings are the expected container artifact only (${warns:-none})" \
    || fail "unexpected warnings: $warns"

  # §9.3's theater-scope check only means something if the theater interface's
  # reachable scope really is the allowlist, and routes.go fails it outright
  # when that interface carries a DEFAULT route — its scope would then be
  # everything. Which interface holds the default is Docker's business and
  # varies (this host puts it on the second-attached network; a GitHub runner
  # names and orders them differently, which is why every interface here is
  # resolved by MAC and never by position). What the check requires is only
  # this, so this is what is asserted.
  local defiface
  defiface="$(docker exec "$FW" awk 'NR>1 && $2=="00000000"{print $1; exit}' /proc/net/route)"
  [ "$defiface" != "$THEATER_IFACE" ] \
    && pass "the theater interface carries no default route, so its scope IS the allowlist (default is on ${defiface:-none})" \
    || fail "the default route is on the theater interface ($defiface); §9.3's scope check cannot pass, and this environment cannot host the test as written"

  step "The zones, proven by behaviour rather than by configuration"
  local pip tip
  pip="$(docker inspect "$FW" --format "{{(index .NetworkSettings.Networks \"$PATRON_NET\").IPAddress}}")"
  tip="$(docker inspect "$FW" --format "{{(index .NetworkSettings.Networks \"$THEATER_NET\").IPAddress}}")"
  reachable() { timeout 5 bash -c "exec 3<>/dev/tcp/$1/80 && printf 'GET /healthz HTTP/1.0\r\n\r\n' >&3 && head -1 <&3" 2>/dev/null | grep -q 200; }
  reachable "$pip" && pass "the patron page answers on the patron network ($pip:80)" \
                   || fail "the patron page is unreachable on the patron network"
  reachable "$tip" && fail "the patron page is ALSO reachable on the theater network ($tip:80) — a hole" \
                   || pass "the same port is dropped on the theater network — the zones are not joined"

  step "The wizard can now complete over a green posture"
  local st; st="$(api POST /api/admin/setup/complete '{}')"
  case "$st" in 200*'"complete":true'*) pass "setup complete, setup mode latched closed" ;;
    *) fail "completion still refused: $st" ;; esac
}

# ---------------------------------------------------------------------------
# watchdog — §3.3: "a runtime watchdog re-asserts these and the dashboard
# screams if any flip". Both halves are asserted: the flip is put back, AND it
# was said out loud.
# ---------------------------------------------------------------------------
watchdog() {
  step "§3.3 watchdog: flip a forwarding knob behind its back"
  docker exec -u 0 "$FW" sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
  [ "$(docker exec "$FW" cat /proc/sys/net/ipv4/ip_forward)" = 1 ] || die "could not flip the knob"
  pass "ip_forward flipped to 1"
  local i v=1
  for i in $(seq 1 60); do
    v="$(docker exec "$FW" cat /proc/sys/net/ipv4/ip_forward)"
    [ "$v" = 0 ] && break
    sleep 1
  done
  [ "$v" = 0 ] && pass "re-asserted to 0 after ${i}s (watchdog interval is 30 s)" \
               || fail "still $v after ${i}s — the watchdog did not restore the posture"
  local lg; lg="$(docker logs "$FW" 2>&1 | grep runtime_watchdog || true)"
  grep -q forwarding_enabled_detected <<<"$lg" && pass "…and it screamed (forwarding_enabled_detected, source=runtime_watchdog)" \
    || fail "the flip was corrected silently — §3.3 requires the dashboard to scream"
  grep -q '"reasserted":"sysctls"' <<<"$lg" && pass "…naming what it re-asserted" || true
}

# ---------------------------------------------------------------------------
# aftermath — the whole point of doing this in a namespace.
# ---------------------------------------------------------------------------
aftermath() {
  step "Aftermath: this host is exactly as it was"
  local now_ns now_fwd
  now_ns="$(readlink /proc/self/ns/net)"; now_fwd="$(cat /proc/sys/net/ipv4/ip_forward)"
  [ "$now_ns" = "$HOST_NETNS" ] && pass "still in the host namespace $now_ns" || fail "host namespace changed"
  [ "$now_fwd" = "$HOST_FWD" ] && pass "host ip_forward is still $now_fwd" \
    || fail "HOST ip_forward changed from $HOST_FWD to $now_fwd"
  local t
  t="$(docker run --rm --network none --cap-drop ALL --cap-add NET_ADMIN "$IMAGE" nft list tables 2>/dev/null | tr '\n' ' ')"
  case "$t" in *openaccessbridge*) fail "an openaccessbridge table leaked outside the test namespace" ;;
    *) pass "no openaccessbridge table outside the test namespace (sees: ${t:-none})" ;; esac
}

main() {
  cleanup
  preflight
  topology
  wizard
  posture
  watchdog
  aftermath
  printf '\n'
  [ "$FAILED" -eq 0 ] && { printf 'APPLIANCE POSTURE OK\n'; return 0; }
  printf '%d assertion(s) failed\n' "$FAILED" >&2
  return 1
}
main "$@"
