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
	InputDeviceID    string         `json:"input_device_id"`
	StreamType       StreamType     `json:"stream_type"`
	SourceChannel    SourceChannel  `json:"source_channel"`
	SampleRate       int            `json:"sample_rate"`
	Channels         int            `json:"channels"`
	LevelDBFS        float64        `json:"level_dbfs"`
	NoiseFloorDBFS   float64        `json:"noise_floor_dbfs"`
	Clipping         bool           `json:"clipping"`
	Silence          bool           `json:"silence"`
	DCOffset         float64        `json:"dc_offset"`
	Status           QualifierState `json:"status"`
	LatencyMSEst     int            `json:"latency_ms_estimate"`
	Present          bool           `json:"present"`
	DeviceConnected  bool           `json:"device_connected"`
	ConnectedClients int            `json:"connected_clients"`
	// Reasons are plain-language explanations for the verdict (§6.3). They carry
	// no audio content.
	Reasons []string `json:"reasons,omitempty"`
}
