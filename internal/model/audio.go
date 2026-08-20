package model

// StreamType names an accessibility audio stream. HI = hearing-impaired,
// VI-N = visually-impaired narrative (audio description).
type StreamType string

const (
	StreamHI  StreamType = "HI"
	StreamVIN StreamType = "VI-N"
)

// SourceChannel selects which captured channel(s) feed a named stream (§6.5).
type SourceChannel string

const (
	ChannelLeft   SourceChannel = "left"
	ChannelRight  SourceChannel = "right"
	ChannelSummed SourceChannel = "summed"
	ChannelStereo SourceChannel = "stereo"
	ChannelMono   SourceChannel = "mono"
)

// QualifierState is the four-state input verdict (§6.3). It must never silently
// accept whatever it is handed (guardrail #7).
type QualifierState string

const (
	QualExcellent QualifierState = "excellent"
	QualUsable    QualifierState = "usable"
	QualMarginal  QualifierState = "marginal"
	QualFailed    QualifierState = "failed"
)

// Usable reports whether a stream in this state should be offered to patrons.
// Marginal is offered with a warning; Failed is never offered.
func (q QualifierState) Usable() bool {
	return q == QualExcellent || q == QualUsable || q == QualMarginal
}

// Rank orders states best→worst for comparisons (0 = best).
func (q QualifierState) Rank() int {
	switch q {
	case QualExcellent:
		return 0
	case QualUsable:
		return 1
	case QualMarginal:
		return 2
	default:
		return 3
	}
}

// AudioStreamState is the transient per-stream audio state (§10.2). It is
// independent of caption state: the system can have valid captions and a failed
// VI feed, or clean HI audio and no captions. Audio samples are CONTENT and are
// never persisted or logged — only these measurements are.
type AudioStreamState struct {
	InputDeviceID  string         `json:"input_device_id"`
	StreamType     StreamType     `json:"stream_type"`
	SourceChannel  SourceChannel  `json:"source_channel"`
	SampleRate     int            `json:"sample_rate"`
	Channels       int            `json:"channels"`
	LevelDBFS      float64        `json:"level_dbfs"`
	NoiseFloorDBFS float64        `json:"noise_floor_dbfs"`
	Clipping       bool           `json:"clipping"`
	Silence        bool           `json:"silence"`
	DCOffset       float64        `json:"dc_offset"`
	Status         QualifierState `json:"status"`
	// LatencyMSEst is the appliance-side ENCODE-PATH estimate in milliseconds
	// (capture period + Opus frame). It is NOT a glass-to-ear figure: the Wi-Fi
	// hop, the browser jitter buffer (the dominant term) and the phone's audio
	// output are outside this appliance and unmeasured. 0 means no encoder ran.
	LatencyMSEst int `json:"latency_ms_estimate"`
	// LatencyMeasured reports whether an end-to-end latency was actually
	// MEASURED. It stays false until the Phase 0C loopback calibration runs on
	// real hardware (BLOCKERS B3, §11.4), so the dashboard can render "not
	// measured" instead of presenting an estimate as a measurement.
	LatencyMeasured  bool `json:"latency_measured"`
	Present          bool `json:"present"`
	DeviceConnected  bool `json:"device_connected"`
	ConnectedClients int  `json:"connected_clients"`
	// Dropouts counts capture periods the input device failed to deliver
	// (§6.3 stability, §6.6 "dropouts").
	Dropouts int `json:"dropouts"`
	// EncoderState is what the media backend is actually doing for this stream
	// (§6.6 "encoder state") — e.g. unknown / unavailable / ready_no_transport.
	EncoderState string `json:"encoder_state"`
	// Reasons are plain-language explanations for the verdict (§6.3). They carry
	// no audio content.
	Reasons []string `json:"reasons,omitempty"`
}

// Encoder states carried on AudioStreamState.EncoderState (§6.6 "encoder
// state"). internal/audio/pipeline produces them; this is their canonical home
// so the dashboard assembler can read a state without importing the media
// pipeline into the caption/web process. internal/invariants asserts the two
// sets of constants stay identical.
const (
	// EncoderStateUnknown: the encode path has not been exercised yet.
	EncoderStateUnknown = "unknown"
	// EncoderStateUnavailable: no encoder ran, so nothing is being encoded for
	// this stream however clean the input measures.
	EncoderStateUnavailable = "unavailable"
	// EncoderStateReadyNoTransport: a real encode was proven locally but there is
	// no subscriber transport yet (B2/B4).
	EncoderStateReadyNoTransport = "ready_no_transport"
)
