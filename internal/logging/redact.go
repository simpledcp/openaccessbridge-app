package logging

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// RedactedPlaceholder replaces any value whose key indicates content.
const RedactedPlaceholder = "[REDACTED]"

// deniedKeySubstrings are field-key substrings that indicate content or
// personally/identifying data which must never reach a log (§9.4). A field whose
// key contains any of these is force-redacted regardless of its value, so a
// caller cannot leak content by mislabeling a field.
var deniedKeySubstrings = []string{
	"text",     // caption text
	"caption_body",
	"cue_text",
	"sample",   // audio samples
	"audio_data",
	"pcm",
	"sdp",      // WebRTC SDP bodies
	"ice_ufrag",
	"ice_pwd",
	"candidate_raw",
	"client_ip",
	"remote_addr",
	"peer_addr",
	"user_agent",
	"password",
	"secret",
	"token",    // session tokens
	"credential",
	"asset_path", // paths can embed title names (§9.4)
	"title",
	"show_name",
}

// isDeniedKey reports whether a field key indicates content/PII and must be
// redacted. Matching is case-insensitive and substring-based so variants are
// caught (e.g. "cue_text", "captionText", "hi_pcm").
func isDeniedKey(key string) bool {
	k := strings.ToLower(key)
	for _, sub := range deniedKeySubstrings {
		if strings.Contains(k, sub) {
			return true
		}
	}
	return false
}

// scrub returns the value to log for a (key, value) pair: the original value if
// the key is safe, or the redaction placeholder if the key is denied. This is
// the single chokepoint every emitted field passes through.
func scrub(key string, value any) any {
	if isDeniedKey(key) {
		return RedactedPlaceholder
	}
	return value
}

// HashID turns a raw identifier (e.g. a client connection ID or show identifier)
// into a short, stable, opaque hex string so events can be correlated WITHOUT
// recording the identifier itself (§9.4 "opaque/hashed IDs"). The salt scopes the
// hash so the same raw value is not linkable across unrelated domains.
func HashID(salt, raw string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + raw))
	return hex.EncodeToString(sum[:6]) // 12 hex chars: enough to correlate, not to reverse
}
