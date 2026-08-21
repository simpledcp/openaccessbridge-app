// Package invariants holds the cross-cutting guardrail assertions for OpenAccessBridge
// (buildplan.md §1.2 guardrails, §11.6 security tests). The per-subsystem packages
// test their own behavior; this suite asserts the SYSTEM-LEVEL invariants the
// kickoff requires to hold from day one:
//
//  1. No persistence of caption text / audio content (memory/tmpfs only).
//  2. No IPv4/IPv6 forwarding; default-deny firewall with no forward-accepts.
//  3. Three supervised processes with privilege separation.
//  4. Independent caption / audio readiness state.
//  5. No silent degradation — failures surface a loud warning and a patron state.
//  6. Nothing sensitive in logs (no caption text / audio / SDP / client IDs).
//
// The tests live in invariants_test.go and run as part of the normal suite (and
// therefore in CI), so a regression that breaches a guardrail fails the build.
package invariants
