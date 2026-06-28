# CaptionBridge deployment

CaptionBridge ships as **three supervised processes** on a **two-NIC** appliance
(buildplan §3.5, §4). This directory holds the systemd units and the
deployment-profile descriptors (`internal/deploy`).

## Processes (systemd)

| Unit | User | Role |
|---|---|---|
| `captionbridge-firewall.service` | root (CAP_NET_ADMIN) | nftables apply + forwarding/isolation verification — the only privileged component |
| `captionbridge-media.service` | `captionbridge` (+audio) | audio capture → qualifier → mapper → Opus → WebRTC |
| `captionbridge-core.service` | `captionbridge` (CAP_NET_BIND_SERVICE) | ACSP state machine, captions, web/admin, supervision |
| `captionbridge.target` | — | groups the three |

`core` **Wants** (not Requires) `media`, so a media crash never drops captions —
captions and audio are independent (§2). `core` **Requires** `firewall`, so the
network posture is verified before patrons are served.

### Install (low-cost/hardened tiers)

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin captionbridge
sudo install -m0755 bin/captionbridge-core bin/captionbridge-media bin/captionbridge-firewall /usr/local/bin/
sudo install -m0644 deploy/systemd/*.service deploy/systemd/*.target /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now captionbridge.target
```

First boot serves the setup wizard (§8.1): force admin-credential creation,
identify theater vs client NICs, set the cinema-server allowlist, pick a firewall
profile, run audio calibration, then run the readiness checks and exit setup.

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
  (`RuntimeDirectory`); only the SQLite *config* store is writable
  (`/var/lib/captionbridge`, config only). **Core dumps are disabled**
  (`LimitCORE=0`) so a crash cannot leak caption text or audio to disk (§9.4).
- **No IP forwarding / default-deny (guardrails #2, #6):** the firewall helper
  sets `net.ipv4.ip_forward=0` + `net.ipv6.conf.all.forwarding=0` and applies a
  default-deny nftables ruleset with an empty `forward` chain, then continuously
  re-verifies by self-introspection (§9.3).
- **Privilege separation (§3.5):** only `firewall` is privileged; `core`/`media`
  run unprivileged with tight `ProtectSystem`, `NoNewPrivileges`, and restricted
  address families.

## Patron Wi-Fi (§9.5)

Production profiles assume a **dedicated external AP** (5 GHz, Wi-Fi 6+),
band-steering controlled, client-isolation configured so phones can still reach
the appliance and mDNS resolves, DHCP scoped to capacity. Run the latency/soak
tests from representative **seats**, not the rack.

> Real hardware bring-up (cinema server, phones, audio jacks) is gated — see
> `../BLOCKERS.md`.
