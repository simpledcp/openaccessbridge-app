// Package logging provides structured observability that emits states, counts,
// durations, error classes, and opaque/hashed IDs — and NEVER caption text,
// audio samples, WebRTC SDP bodies, or client identifiers (buildplan.md §9.4).
//
// The redaction guard (redact.go) is enforced at the field layer so a caller
// cannot accidentally leak content even by mislabeling a field. Tests assert the
// invariant.
package logging

// Event is a stable, named observability event keyed on a state transition,
// count, duration, or error class (§9.4). The taxonomy below is the documented
// minimum set; callers may add events following the same dotted convention.
type Event string

const (
	// Caption plane.
	EvACSPConnected         Event = "caption.acsp.connected"
	EvACSPDisconnected      Event = "caption.acsp.disconnected"
	EvLeaseGranted          Event = "caption.lease.granted"
	EvLeaseExpiring         Event = "caption.lease.expiring"
	EvLeaseExpired          Event = "caption.lease.expired"
	EvTimelineAnchorUpdated Event = "caption.timeline.anchor_updated"
	EvRPLReceived           Event = "caption.rpl.received"
	EvAssetFetchFailed      Event = "caption.asset.fetch_failed"
	EvAssetFetched          Event = "caption.asset.fetched"
	// CueVisibleNoText records THAT a cue was shown and for how long, never its
	// words (§9.4) — enough to debug a "captions look wrong" report.
	EvCueVisibleNoText Event = "caption.cue.visible_no_text"
	EvStateTransition  Event = "caption.state.transition"
	EvOutputModeChange Event = "caption.output_mode.changed"

	// Audio plane.
	EvAudioInputConnected    Event = "audio.input.connected"
	EvAudioInputDisconnected Event = "audio.input.disconnected"
	EvAudioLevelClipping     Event = "audio.level.clipping"
	EvAudioSilenceSustained  Event = "audio.silence.sustained"
	EvAudioQualifierVerdict  Event = "audio.qualifier.verdict"
	// EvAudioEncoderStopped records that a stream's live Opus encode ended, with
	// the error CLASS that ended it — never a command line or an SDP body.
	EvAudioEncoderStopped Event = "audio.encoder.stopped"
	// EvAudioEncoderDropped counts mapped audio blocks the encoder could not
	// take. It is a count of a patron-audible gap, not any audio content.
	EvAudioEncoderDropped Event = "audio.encoder.frames_dropped"

	// WebRTC.
	EvWebRTCPeerConnected Event = "webrtc.peer.connected"
	EvWebRTCPeerReconnect Event = "webrtc.peer.reconnect"
	EvWebRTCPeerClosed    Event = "webrtc.peer.closed"

	// Security / firewall.
	EvForwardingEnabledDetected Event = "security.forwarding_enabled_detected"
	EvFirewallProfileApplied    Event = "firewall.profile.applied"
	EvVerificationFailed        Event = "security.verification_failed"
	EvVerificationPassed        Event = "security.verification_passed"
	EvServerIdentityMismatch    Event = "security.server_identity_mismatch"

	// Debug capture (§9.4). The audit trail for the one sanctioned way content
	// may be written down: who granted it (hashed), its bounds, and its end.
	// Emitted by internal/logging/debugcapture; the content itself never passes
	// through the logger.
	EvDebugCaptureStarted Event = "security.debug_capture.started"
	EvDebugCaptureStopped Event = "security.debug_capture.stopped"
	EvDebugCaptureCapped  Event = "security.debug_capture.limit_reached"

	// §9.4 support sessions: the only re-entry path once SSH is disabled, so
	// every transition is an audit event at Warn — the level a review reads. The
	// operator's REASON never appears here: it is free text and could contain
	// anything, so only its length travels (the text itself is in the local audit
	// trail). The grantee is hashed, as everywhere else in this package.
	EvSupportSessionOpened Event = "security.support_mode.opened"
	EvSupportSessionClosed Event = "security.support_mode.closed"

	// Process supervision.
	EvProcessStarted   Event = "process.started"
	EvProcessRestarted Event = "process.restarted"
	EvProcessUnhealthy Event = "process.unhealthy"
)
