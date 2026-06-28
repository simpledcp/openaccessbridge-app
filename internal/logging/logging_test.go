package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRedaction_DeniedKeysScrubbed asserts the core §9.4 invariant: any field
// whose key indicates content/PII is replaced with the placeholder, regardless of
// the value supplied. This guards against accidental leakage of caption text,
// audio samples, SDP, tokens, client IPs, and title-bearing asset paths.
func TestRedaction_DeniedKeysScrubbed(t *testing.T) {
	secret := "THE QUICK BROWN FOX — a secret caption line"
	cases := []struct {
		key string
		val any
	}{
		{"cue_text", secret},
		{"captionText", secret},
		{"text", secret},
		{"hi_pcm", []byte("rawpcm")},
		{"audio_data", secret},
		{"sdp", "v=0\r\no=- 123 ..."},
		{"answer_sdp", "v=0"},
		{"client_ip", "10.0.0.5"},
		{"remote_addr", "10.0.0.5:55555"},
		{"user_agent", "Mozilla/5.0"},
		{"session_token", "abc.def.ghi"},
		{"password", "hunter2"},
		{"asset_path", "/mnt/Avengers_Endgame_reel1.mxf"},
		{"show_title", "Some Movie"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		l := New(Options{Component: "test", Writer: &buf, JSON: true})
		l.Info(EvCueVisibleNoText, Field(c.key, c.val))
		out := buf.String()
		if strings.Contains(out, "secret") || strings.Contains(out, "rawpcm") ||
			strings.Contains(out, "hunter2") || strings.Contains(out, "Avengers") ||
			strings.Contains(out, "Some Movie") || strings.Contains(out, "Mozilla") ||
			strings.Contains(out, "10.0.0.5") || strings.Contains(out, "abc.def.ghi") ||
			strings.Contains(out, "v=0") {
			t.Fatalf("denied key %q leaked content into log: %s", c.key, out)
		}
		if !strings.Contains(out, RedactedPlaceholder) {
			t.Fatalf("denied key %q not redacted: %s", c.key, out)
		}
	}
}

// TestRedaction_SafeKeysPreserved asserts safe state/count fields are NOT
// redacted, so observability remains useful.
func TestRedaction_SafeKeysPreserved(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Component: "core", Writer: &buf, JSON: true})
	l.Info(EvLeaseGranted,
		Field("lease_ms", 30000),
		Field("state", "LEASED"),
		Field("client_count", 4),
		Field("opaque_id", HashID("client", "192.168.1.50")),
	)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line not valid JSON: %v\n%s", err, buf.String())
	}
	if m["event"] != string(EvLeaseGranted) {
		t.Errorf("event = %v, want %v", m["event"], EvLeaseGranted)
	}
	if m["component"] != "core" {
		t.Errorf("component = %v, want core", m["component"])
	}
	if m["state"] != "LEASED" {
		t.Errorf("state field was altered: %v", m["state"])
	}
	if m["lease_ms"] != float64(30000) {
		t.Errorf("lease_ms = %v, want 30000", m["lease_ms"])
	}
}

// TestHashID_OpaqueStableScoped asserts hashed IDs are stable, opaque (do not
// contain the raw value), and salt-scoped (not linkable across domains).
func TestHashID_OpaqueStableScoped(t *testing.T) {
	raw := "192.168.1.50"
	a := HashID("client", raw)
	b := HashID("client", raw)
	if a != b {
		t.Errorf("HashID not stable: %q vs %q", a, b)
	}
	if strings.Contains(a, raw) || strings.Contains(a, "192") {
		t.Errorf("HashID leaked raw value: %q", a)
	}
	if HashID("show", raw) == a {
		t.Errorf("HashID not salt-scoped: same hash across domains")
	}
	if len(a) != 12 {
		t.Errorf("HashID length = %d, want 12", len(a))
	}
}
