#!/usr/bin/env bash
# Container smoke test for the OpenAccessBridge image (run by `make docker-smoke`
# and CI). It brings the three-container dev stack up from
# $OPENACCESSBRIDGE_IMAGE, waits for every service to report healthy, checks
# what a patron and an operator would first touch, exercises the single-
# container demo, and fails on any ERROR-level log line. It never needs root,
# host networking, or audio hardware, so it can run on any Docker host.
#
# What it proves is the container PACKAGING: the image runs, the three
# processes find each other over the shared tmpfs socket directory across
# container boundaries under two different accounts, and the surfaces serve.
# It proves nothing about the appliance's network posture (that needs the
# host-network stack on a real two-NIC box) or about hardware (BLOCKERS.md).
set -euo pipefail

IMAGE="${OPENACCESSBRIDGE_IMAGE:?set OPENACCESSBRIDGE_IMAGE to the image under test}"
PATRON_PORT="${OAB_PATRON_PORT:-18080}"
ADMIN_PORT="${OAB_ADMIN_PORT:-18090}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE=(docker compose -f "$HERE/docker-compose.dev.yml")
export OPENACCESSBRIDGE_IMAGE="$IMAGE" OAB_PATRON_PORT="$PATRON_PORT" OAB_ADMIN_PORT="$ADMIN_PORT"

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
cleanup() {
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  docker rm -f oab-smoke-demo >/dev/null 2>&1 || true
}
trap cleanup EXIT

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" || true; }

echo "== stack: $IMAGE on ports $PATRON_PORT/$ADMIN_PORT"
cleanup
"${COMPOSE[@]}" up -d --no-build

# Every one of the three services must report healthy — the healthchecks are
# the sockets and GET /healthz, so "healthy" means the processes are up and
# listening. Named explicitly: a container that exited is not listed by
# `ps`, and "everything listed is healthy" must not pass over an absent one.
for _ in $(seq 1 30); do
  unhealthy=""
  for svc in firewall media core; do
    h=$("${COMPOSE[@]}" ps -a --format '{{.Service}} {{.State}} {{.Health}}' | awk -v s="$svc" '$1==s{print $2"/"$3}')
    [ "$h" = "running/healthy" ] || unhealthy="$unhealthy $svc=${h:-absent}"
  done
  [ -z "$unhealthy" ] && break
  sleep 2
done
if [ -n "$unhealthy" ]; then
  "${COMPOSE[@]}" ps -a
  "${COMPOSE[@]}" logs --no-color | tail -50
  fail "services not healthy:$unhealthy"
fi
echo "== all three services healthy"

# Healthy is not enough: a helper that died on the shared directory and was
# restarted into a working state would otherwise pass. Nobody may have restarted.
for id in $("${COMPOSE[@]}" ps -a -q); do
  rc=$(docker inspect -f '{{.RestartCount}}' "$id")
  [ "$rc" = 0 ] || { docker logs "$id" 2>&1 | tail -20; fail "$(docker inspect -f '{{.Name}}' "$id") restarted $rc time(s)"; }
done
echo "== no container restarted"

[ "$(http_code "http://127.0.0.1:$PATRON_PORT/healthz")" = 200 ] || fail "patron /healthz"
[ "$(http_code "http://127.0.0.1:$PATRON_PORT/accessibility")" = 200 ] || fail "patron /accessibility"
[ "$(http_code "http://127.0.0.1:$ADMIN_PORT/healthz")" = 200 ] || fail "admin /healthz"
# The setup wizard is what an operator meets first (§8.1). On a fresh config
# volume no credential exists yet, so the wizard's state is served openly until
# one is created (web.requireAdmin); the dashboard proper must still refuse.
[ "$(http_code "http://127.0.0.1:$ADMIN_PORT/api/admin/setup/state")" = 200 ] || fail "setup wizard state not served at first boot"
[ "$(http_code "http://127.0.0.1:$ADMIN_PORT/admin/setup")" = 200 ] || fail "setup wizard page not served at first boot"
# Captions: the demo show offers languages. Audio: the media process answered
# core over the cross-container socket (the streams list comes from it).
langs=$(curl -s --max-time 5 "http://127.0.0.1:$PATRON_PORT/api/caption/languages")
echo "$langs" | grep -q '"code"' || fail "no caption languages offered: $langs"
streams=$(curl -s --max-time 5 "http://127.0.0.1:$PATRON_PORT/api/audio/streams")
echo "$streams" | grep -q '"playback_available":true' || fail "audio not available (media unreachable or no encoder): $streams"
echo "== patron + admin surfaces answer; captions and audio offered"

# core's OWN view of its helpers — §3.5's per-process liveness, which is what
# the dashboard shows — read from the admin status the wizard serves openly at
# first boot. Both helpers were reached across container boundaries and under a
# different account from core's, or this line fails.
status=$(curl -s --max-time 5 "http://127.0.0.1:$ADMIN_PORT/api/admin/status")
echo "$status" | grep -q '"name":"openaccessbridge-firewall","alive":true' || fail "core does not see the firewall helper alive: $status"
echo "$status" | grep -q '"name":"openaccessbridge-media","alive":true' || fail "core does not see the media helper alive: $status"
echo "== core reports both helpers alive"

# The shared socket directory: provisioned root-owned, group-writable and
# sticky, left that way by all three, holding BOTH helpers' sockets with the
# modes the units specify — owner-only for media, group for the root helper (§3.5).
dirmode=$("${COMPOSE[@]}" exec -T core stat -c '%U:%G %a' /run/openaccessbridge)
[ "$dirmode" = "root:openaccessbridge 1770" ] || fail "shared runtime dir is '$dirmode', want root:openaccessbridge 1770"
socks=$("${COMPOSE[@]}" exec -T core ls -l /run/openaccessbridge)
echo "$socks" | grep -q 'srw-rw---- .* firewall.sock' || fail "firewall socket missing or wrong mode: $socks"
echo "$socks" | grep -q 'srw------- .* media.sock' || fail "media socket missing or wrong mode: $socks"
echo "== shared runtime dir: root:openaccessbridge 1770, both sockets with the unit modes"

# A helper restart into the shared directory must be clean: the stale socket is
# replaced and core sees the helper again.
"${COMPOSE[@]}" restart firewall >/dev/null
for _ in $(seq 1 15); do
  status=$(curl -s --max-time 5 "http://127.0.0.1:$ADMIN_PORT/api/admin/status")
  echo "$status" | grep -q '"name":"openaccessbridge-firewall","alive":true' && break
  sleep 2
done
echo "$status" | grep -q '"name":"openaccessbridge-firewall","alive":true' || fail "firewall helper not seen alive after a restart: $status"
echo "== helper restarts cleanly into the shared dir"

if "${COMPOSE[@]}" logs --no-color | grep -q '"level":"ERROR"'; then
  "${COMPOSE[@]}" logs --no-color | grep '"level":"ERROR"' | head -20
  fail "ERROR-level log lines"
fi
echo "== no ERROR-level log lines"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null

# The single-container demo: all three processes, then a clean stop.
demo_port=$((PATRON_PORT + 1))
docker run -d --name oab-smoke-demo -p "127.0.0.1:$demo_port:8080" "$IMAGE" openaccessbridge-demo >/dev/null
for _ in $(seq 1 15); do
  [ "$(http_code "http://127.0.0.1:$demo_port/healthz")" = 200 ] && break
  sleep 2
done
[ "$(http_code "http://127.0.0.1:$demo_port/healthz")" = 200 ] || { docker logs oab-smoke-demo | tail -20; fail "demo container /healthz"; }
docker stop -t 10 oab-smoke-demo >/dev/null
docker logs oab-smoke-demo 2>&1 | grep -q '"phase":"shutdown"' || fail "demo container did not shut down cleanly on SIGTERM"
echo "== single-container demo serves and stops cleanly"
echo "SMOKE OK"
