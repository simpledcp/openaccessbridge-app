package qr

// The Wi-Fi onboarding payload (§9.5: "QR onboarding carries SSID + password +
// URL where the platform allows, so a patron joins the network and the page in
// one scan").
//
// IT IS A SECOND CODE, not a richer version of the join URL. One QR cannot be
// both a network join and a URL: the `WIFI:` form is a distinct grammar that
// phones hand to their Wi-Fi settings, and a URL is a URL. A venue that wants
// one-scan onboarding prints both — network first, then the page — and the
// appliance renders each.
//
// THE FORMAT IS DE FACTO, NOT A STANDARD, which is worth stating because it
// changes how the escaping is treated. There is no ISO document for `WIFI:`; the
// grammar comes from ZXing and is what phone cameras implement. So the rules
// below are transcribed from that grammar and, like the QR encoder itself, are
// checked against an independent implementation rather than against our reading
// of them.

import "strings"

// WiFiSecurity is the authentication a patron network uses.
type WiFiSecurity string

const (
	// WiFiWPA covers WPA, WPA2 and WPA3 — the grammar has one token for all of
	// them, and phones treat it as "there is a passphrase".
	WiFiWPA WiFiSecurity = "WPA"
	// WiFiWEP exists because the grammar has it. §9.5 asks for 5 GHz and Wi-Fi 6;
	// a venue offering WEP has a bigger problem than onboarding.
	WiFiWEP WiFiSecurity = "WEP"
	// WiFiOpen is an open network: no passphrase at all.
	WiFiOpen WiFiSecurity = "nopass"
)

// WiFiNetwork is the patron network a QR can join a phone to.
type WiFiNetwork struct {
	SSID     string
	Password string
	Security WiFiSecurity
	// Hidden marks a network that does not broadcast its SSID. A phone needs to
	// be told, or it will not find a network it was just handed the name of.
	Hidden bool
}

// WiFiPayload renders the network as the `WIFI:` string a QR carries.
//
// Two escaping rules, and both are the kind of thing that works for every SSID
// anyone tests with and then fails at one venue:
//
//   - `\`, `;`, `:` and `"` are structural in this grammar, so an SSID or
//     passphrase containing any of them is escaped. A cinema passphrase of
//     "popcorn;2026" would otherwise terminate the field and the phone would
//     join with the wrong password — or fail to parse the code at all.
//
//     The COMMA is deliberately NOT escaped, and that is a correction. The first
//     version of this escaped it too, reasoning that it is separator-ish in the
//     related MECARD contact grammar. It is not a field terminator here, and an
//     UNNECESSARY escape is the more dangerous mistake: a parser that does not
//     strip it hands the phone a passphrase with a literal backslash in it and
//     the join fails, whereas omitting an escape for a character that never
//     terminates a field costs nothing. ZXing's parser and segno agree on the
//     four characters above; the disagreement was ours.
//
// A THIRD RULE IS DELIBERATELY NOT APPLIED, and the reasoning matters more than
// the rule. ZXing's documentation says a value made entirely of hex digits should
// be wrapped in double quotes, because an unquoted hex string may be read as raw
// bytes rather than as text. But quoting is the same gamble as escaping a comma,
// pointed the other way: a parser that does not strip the quotes hands the phone
// a passphrase with literal quote marks in it, and the join fails. Both readings
// break for someone, neither can be tested here (no phones, BLOCKER B2), and the
// independent implementation does not quote.
//
// So this does not quote, and instead REPORTS the ambiguity where a venue can
// act on it: AllHexAmbiguity flags an all-hex value so the operator surface can
// suggest a passphrase with one non-hex character in it, which sidesteps the
// question entirely. Guessing silently on a patron's behalf is the one option
// that helps nobody.
func WiFiPayload(n WiFiNetwork) string {
	sec := n.Security
	if sec == "" {
		if strings.TrimSpace(n.Password) == "" {
			sec = WiFiOpen
		} else {
			sec = WiFiWPA
		}
	}
	var b strings.Builder
	b.WriteString("WIFI:")
	b.WriteString("T:" + string(sec) + ";")
	b.WriteString("S:" + escapeWiFi(n.SSID) + ";")
	if sec != WiFiOpen {
		b.WriteString("P:" + escapeWiFi(n.Password) + ";")
	}
	if n.Hidden {
		b.WriteString("H:true;")
	}
	b.WriteString(";")
	return b.String()
}

// escapeWiFi applies the grammar's escaping to one field value.
func escapeWiFi(v string) string {
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\', ';', ':', '"':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// AllHexAmbiguity reports whether this network's SSID or passphrase is entirely
// hexadecimal, which some scanners read as raw bytes rather than as text.
//
// It is a REPORT rather than a fix because both available fixes break for
// somebody (see WiFiPayload), and because the venue has an option neither of
// them has: change one character. The operator surface says so.
func (n WiFiNetwork) AllHexAmbiguity() bool {
	return isAllHex(strings.TrimSpace(n.SSID)) || isAllHex(strings.TrimSpace(n.Password))
}

// isAllHex reports whether every character is a hex digit — the case the
// grammar would otherwise read as bytes.
func isAllHex(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// Configured reports whether there is enough to render a joinable code. An SSID
// is the minimum; a secured network also needs its passphrase, and rendering one
// without it would produce a code that fails at the moment a patron scans it.
func (n WiFiNetwork) Configured() bool {
	if strings.TrimSpace(n.SSID) == "" {
		return false
	}
	if n.Security != WiFiOpen && strings.TrimSpace(n.Password) == "" {
		return false
	}
	return true
}
