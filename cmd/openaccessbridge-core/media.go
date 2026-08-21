package main

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/ipc"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// mediaClient is core's half of the loopback link to openaccessbridge-media (§3.5):
// it pushes the stored auditorium input profile (§10.5) and polls the per-stream
// audio state. It also measures the helper's liveness, because "the helper is
// gone" and "every input is silent" are different faults with the same symptom
// on an audio-only dashboard (§3.5, §12.2).
type mediaClient struct {
	sockPath string
	log      *logging.Logger

	mu sync.Mutex
	// latest is the last per-stream state the helper reported. It is cleared when
	// the helper stops answering: a stale reading presented as current is exactly
	// the frozen verdict guardrail #7 forbids.
	latest []model.AudioStreamState
	alive  bool
	// lastSeen is nil until the helper first answers.
	lastSeen *time.Time
	detail   string
	// configuredKey is the input profile the helper has been given. It is cleared
	// whenever a request fails, so a helper that restarted (and reverted to its
	// own defaults) is re-configured rather than assumed to remember.
	configuredKey string
	configureErr  string
	// scope is the media process's OWN account of its ICE gathering, refreshed on
	// every push. Core does not remember what it sent: a media process that
	// restarted came back unconfined, and an appliance that trusted its own last
	// write would keep reporting a confinement that had gone away (§8.3).
	scope    model.ICEScopeReport
	scopeErr string
}

func newMediaClient(sockPath string, log *logging.Logger) *mediaClient {
	return &mediaClient{sockPath: sockPath, log: log}
}

// configure pushes the auditorium input profile when it has changed. §10.5's
// input_device_id and channel_mapping are configuration the media process
// consumes; a profile with no mapping configured yet is left alone rather than
// pushed as an empty one.
//
// It reports whether the media plan was just rebuilt. That matters to the
// caller: the streams have only this instant been (re)started, so anything they
// report about the input for the next moment describes the RESTART, not the
// booth's signal.
func (c *mediaClient) configure(prof model.AuditoriumProfile, ok bool) (reconfigured bool) {
	if !ok || prof.ChannelMapping == "" {
		return false
	}
	key := prof.InputDeviceID + "|" + prof.ChannelMapping
	c.mu.Lock()
	unchanged := key == c.configuredKey
	c.mu.Unlock()
	if unchanged {
		return false
	}

	payload, err := json.Marshal(prof)
	if err != nil {
		return false
	}
	resp, err := c.request(ipc.Message{Type: ipc.TypeMediaConfigure, ID: "configure", Payload: payload})
	if err != nil {
		c.noteDown(err.Error())
		return false
	}
	var res model.MediaCommandResult
	_ = json.Unmarshal(resp.Payload, &res)

	c.mu.Lock()
	defer c.mu.Unlock()
	if !res.OK {
		// A refusal is loud: the booth's configured input is NOT running, whatever
		// the previous plan happens to be doing (guardrail #7).
		c.configureErr, c.configuredKey = res.Error, ""
		// A class in the log, the detail in the operator-facing warning: the
		// helper's reason can quote the stored mapping string, which is operator
		// input (§9.4).
		c.log.Error(logging.EvProcessUnhealthy,
			logging.Field("phase", "media_configure_refused"),
			logging.Field("reason", "input_profile_rejected"))
		return false
	}
	c.configureErr, c.configuredKey = "", key
	// The reply's stream list is deliberately NOT adopted as the latest state:
	// those streams were built microseconds ago and have not qualified anything
	// yet. The next poll reports a measurement instead of a placeholder.
	c.log.Info(logging.EvStateTransition,
		logging.Field("phase", "media_configured"),
		logging.Field("device_id", prof.InputDeviceID),
		logging.Field("mapping", prof.ChannelMapping))
	return true
}

// applyPatronScope pushes the §9.1 client interface down to the media process,
// which confines ICE gathering to it (DECISIONS D27, internal/audio/webrtcpeer).
//
// It is pushed on EVERY cycle rather than only on change, and the reply's report
// is what core keeps. Pushing once and remembering the answer would survive
// exactly until the media process restarted: it would come back gathering on
// every interface while the dashboard went on saying "confined".
//
// An empty name is a legitimate state — the setup wizard has not run — and is
// pushed as "unconfined" rather than skipped, so the appliance cannot end up in
// a scope no operator chose.
func (c *mediaClient) applyPatronScope(iface string) {
	payload, err := json.Marshal(ipc.MediaNetworkScope{PatronInterface: iface})
	if err != nil {
		return
	}
	resp, err := c.request(ipc.Message{Type: ipc.TypeMediaNetwork, ID: "network", Payload: payload})
	if err != nil {
		c.noteDown(err.Error())
		return
	}
	var res ipc.MediaNetworkResult
	_ = json.Unmarshal(resp.Payload, &res)

	c.mu.Lock()
	defer c.mu.Unlock()
	// Logged on the TRANSITION into refusal, not on every cycle: this runs on the
	// dashboard's poll interval, and a persistent misconfiguration would
	// otherwise fill the log with one line per tick for the length of a show.
	if res.Error != "" && c.scopeErr != res.Error {
		c.log.Error(logging.EvProcessUnhealthy,
			logging.Field("phase", "patron_scope_refused"),
			logging.Field("interface", iface))
	}
	c.scope, c.scopeErr = res.Report, res.Error
}

// iceScope is the media peer's measured gathering report, for the dashboard.
func (c *mediaClient) iceScope() model.ICEScopeReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scope
}

// poll refreshes the per-stream audio state and the helper's liveness.
func (c *mediaClient) poll() {
	resp, err := c.request(ipc.Message{Type: ipc.TypeMediaStatus, ID: "status"})
	if err != nil {
		c.noteDown(err.Error())
		return
	}
	var states []model.AudioStreamState
	_ = json.Unmarshal(resp.Payload, &states)

	now := time.Now()
	c.mu.Lock()
	c.latest, c.alive, c.lastSeen, c.detail = states, true, &now, ""
	c.mu.Unlock()
}

func (c *mediaClient) request(m ipc.Message) (ipc.Message, error) {
	cli, err := ipc.Dial(c.sockPath)
	if err != nil {
		return ipc.Message{}, err
	}
	defer cli.Close()
	return cli.Request(m)
}

// noteDown records that the helper is not answering. The audio state is dropped
// (there is nothing current to report) and the pushed configuration is forgotten
// so a restarted helper gets it again.
func (c *mediaClient) noteDown(detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alive {
		c.log.Warn(logging.EvProcessUnhealthy,
			logging.Field("process", model.ProcessMedia),
			logging.Field("reason", "loopback_request_failed"))
	}
	c.latest, c.alive, c.configuredKey = nil, false, ""
	// The scope report goes with it: a report from a process that is no longer
	// answering describes an appliance that may not be gathering at all.
	c.scope, c.scopeErr = model.ICEScopeReport{}, ""
	c.detail = "request_failed"
	_ = detail // the transport error text is not surfaced: it can name paths (§9.4)
}

func (c *mediaClient) audio() []model.AudioStreamState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

func (c *mediaClient) liveness() model.ProcessLiveness {
	c.mu.Lock()
	defer c.mu.Unlock()
	return model.ProcessLiveness{
		Name: model.ProcessMedia, Alive: c.alive, LastSeen: c.lastSeen, Detail: c.detail,
	}
}

// warnings reports the media-side conditions core itself observed, as opposed to
// the ones derivable from the stream states.
func (c *mediaClient) warnings() []model.Warning {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []model.Warning
	if c.configureErr != "" {
		out = append(out, model.Warning{
			Code:     model.WarnAudioInputProfileRejected,
			Message:  "The media process refused this auditorium's configured audio input: " + c.configureErr,
			Severity: model.SeverityCritical,
		})
	}
	if c.scopeErr != "" {
		// The patron interface on file cannot be gathered on, so every patron who
		// tries to listen is refused. That is a show-stopping condition for audio
		// and it is invisible from the stream states, which go on qualifying an
		// input nobody can reach (guardrail #7).
		out = append(out, model.Warning{
			Code:     model.WarnPatronInterfaceUnusable,
			Message:  "Patron audio cannot be served: " + c.scopeErr,
			Severity: model.SeverityCritical,
		})
	}
	return out
}
