package qr

import (
	"strings"
	"testing"
)

// The ordinary case: a venue's patron network, WPA, one scan to join.
func TestAnOrdinaryPatronNetworkRenders(t *testing.T) {
	got := WiFiPayload(WiFiNetwork{SSID: "CinemaPatron", Password: "popcorn2026", Security: WiFiWPA})
	want := "WIFI:T:WPA;S:CinemaPatron;P:popcorn2026;;"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// The escaping rule that works for every SSID anyone tests with and then fails at
// one venue. A passphrase containing a semicolon would otherwise terminate the
// field: the phone joins with the wrong password, or cannot parse the code.
func TestStructuralCharactersAreEscaped(t *testing.T) {
	got := WiFiPayload(WiFiNetwork{
		SSID:     `Screen 3; Patron`,
		Password: `a\b;c,d:e"f`,
		Security: WiFiWPA,
	})
	if !strings.Contains(got, `S:Screen 3\; Patron;`) {
		t.Errorf("the SSID's semicolon is not escaped: %q", got)
	}
	if !strings.Contains(got, `P:a\\b\;c,d\:e\"f;`) {
		t.Errorf("the passphrase is not escaped correctly: %q", got)
	}
}

// The comma must NOT be escaped. It is not a field terminator in this grammar,
// and an unnecessary escape is the more dangerous error: a parser that does not
// strip it hands the phone a passphrase containing a literal backslash, and the
// join fails at the moment a patron scans. Checked against the two independent
// implementations that agree here (ZXing's parser and segno).
func TestACommaIsNotEscaped(t *testing.T) {
	got := WiFiPayload(WiFiNetwork{SSID: "Patron", Password: "one,two", Security: WiFiWPA})
	if strings.Contains(got, `\,`) {
		t.Fatalf("the comma was escaped, which would put a backslash in the passphrase: %q", got)
	}
	if !strings.Contains(got, "P:one,two;") {
		t.Fatalf("the passphrase did not survive intact: %q", got)
	}
}

// An all-hex value is NOT quoted — and is flagged instead.
//
// ZXing's documentation says to quote it, so that a scanner does not read the
// hex as raw bytes. But quoting is the same gamble as escaping a comma pointed
// the other way: a parser that does not strip the quotes puts literal quote
// marks in the passphrase. Both readings break for someone and neither can be
// tested here (B2), so the appliance reports the ambiguity to the one party who
// can remove it: the venue, by changing one character.
func TestAnAllHexValueIsFlaggedRatherThanGuessedAt(t *testing.T) {
	n := WiFiNetwork{SSID: "Patron", Password: "1234567890abcdef", Security: WiFiWPA}
	got := WiFiPayload(n)
	if strings.Contains(got, `"`) {
		t.Fatalf("an all-hex passphrase was quoted, which some parsers hand to the phone verbatim: %q", got)
	}
	if !n.AllHexAmbiguity() {
		t.Fatal("an all-hex passphrase was not flagged, so nobody can act on it")
	}
	// A passphrase with one non-hex character has no ambiguity at all.
	if (WiFiNetwork{SSID: "Patron", Password: "1234567890abcdefz"}).AllHexAmbiguity() {
		t.Error("a passphrase with a non-hex character was flagged")
	}
	if (WiFiNetwork{SSID: "Patron", Password: "cafe latte"}).AllHexAmbiguity() {
		t.Error("a passphrase with a space was flagged")
	}
	// An SSID can hit it too.
	if !(WiFiNetwork{SSID: "abc123", Password: "popcorn"}).AllHexAmbiguity() {
		t.Error("an all-hex SSID was not flagged")
	}
}

// An open network carries no passphrase field at all: an empty P: on an open
// network makes some phones prompt for a password that does not exist.
func TestAnOpenNetworkCarriesNoPassphraseField(t *testing.T) {
	got := WiFiPayload(WiFiNetwork{SSID: "CinemaGuest", Security: WiFiOpen})
	if strings.Contains(got, "P:") {
		t.Fatalf("an open network carries a passphrase field: %q", got)
	}
	if !strings.Contains(got, "T:nopass;") {
		t.Fatalf("an open network is not marked nopass: %q", got)
	}
}

// A hidden network must be marked, or a phone will not find the network whose
// name it was just handed.
func TestAHiddenNetworkIsMarked(t *testing.T) {
	got := WiFiPayload(WiFiNetwork{SSID: "Patron", Password: "pw", Security: WiFiWPA, Hidden: true})
	if !strings.Contains(got, "H:true;") {
		t.Fatalf("a hidden network is not marked: %q", got)
	}
}

// Rendering a secured network with no passphrase would produce a code that fails
// at the moment a patron scans it, which is the worst place to find out.
func TestASecuredNetworkWithNoPassphraseIsNotConsideredConfigured(t *testing.T) {
	if (WiFiNetwork{SSID: "Patron", Security: WiFiWPA}).Configured() {
		t.Error("a WPA network with no passphrase reports as configured")
	}
	if (WiFiNetwork{Security: WiFiOpen}).Configured() {
		t.Error("a network with no SSID reports as configured")
	}
	if !(WiFiNetwork{SSID: "Patron", Security: WiFiOpen}).Configured() {
		t.Error("an open network with an SSID is not configured")
	}
	if !(WiFiNetwork{SSID: "Patron", Password: "pw", Security: WiFiWPA}).Configured() {
		t.Error("a complete WPA network is not configured")
	}
}

// The payload has to survive the encoder: a Wi-Fi code is longer than it looks
// once escaped, and it still has to fit a symbol a phone can read.
func TestAWiFiPayloadEncodesToAScannableSymbol(t *testing.T) {
	payload := WiFiPayload(WiFiNetwork{
		SSID: "Auditorium 3 — Patron", Password: `a\b;c,d:e"f`, Security: WiFiWPA,
	})
	sym, err := Encode([]byte(payload), DefaultECC)
	if err != nil {
		t.Fatalf("a Wi-Fi payload could not be encoded: %v", err)
	}
	if sym.Version > 10 {
		t.Fatalf("a Wi-Fi code came out at version %d (%d modules); a lobby sign would need to be huge",
			sym.Version, sym.Modules())
	}
}
