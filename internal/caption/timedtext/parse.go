// Package timedtext parses D-Cinema timed-text XML into the normalized cue model
// (buildplan.md §5.4). It handles the ST 428-7 base subtitle grammar, the
// ST 428-10 closed-caption/closed-subtitle profile (which builds on and
// constrains 428-7), and legacy Interop (CineCanvas) subtitle XML.
//
// Phase-1 priority is correct TEXT, correct LANGUAGE, and correct timeline-relative
// START/END timing (edit-rate/timecode → ms). Styling and positioning are carried
// as opaque pass-through fields but not interpreted (§5.4).
//
// The track type (ClosedCaption vs ClosedSubtitle vs on-screen) is authoritatively
// declared at the ST 429-12 / RPL level, not inside the timed-text XML, so Parse
// leaves Cue.Kind unset (TrackUnknown); the RPL layer stamps it.
//
// PROVISIONAL: grammars are implemented from public standard summaries; reconcile
// against the licensed ST 428-7/428-10 and Interop specs (§1.4).
package timedtext

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/simpledcp/captionbridge/internal/model"
)

// Format identifies the detected timed-text grammar.
type Format string

const (
	FormatST428    Format = "smpte_st428"  // ST 428-7 / 428-10 (SubtitleReel)
	FormatInterop  Format = "interop"      // legacy CineCanvas (DCSubtitle)
	FormatUnknown  Format = "unknown"
)

// Document is a parsed timed-text document with all cue times normalized to
// timeline-relative milliseconds.
type Document struct {
	Format       Format
	Language     string
	EditRate     EditRate
	TimeCodeRate float64 // editable units (frames/ticks) per second used for conversion
	Cues         []model.Cue
	// Warnings record non-fatal parse issues as classes/counts — never content.
	Warnings []string

	// raw stages cues before time resolution (rate may appear after subtitles in
	// malformed files); drained by resolveTimes.
	raw []rawCue
}

// Parse reads timed-text XML from r and returns a normalized Document. It returns
// an error only for unrecoverable structural failures (malformed XML, unknown
// root); individual unparseable cues are skipped and recorded in Warnings so a
// single bad cue degrades to "no caption" rather than failing the document.
func Parse(r io.Reader) (*Document, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read timed text: %w", err)
	}
	return ParseBytes(b)
}

// ParseBytes parses timed-text XML from a byte slice.
func ParseBytes(b []byte) (*Document, error) {
	doc := &Document{Format: FormatUnknown}

	// First pass: sniff the root element to choose the grammar.
	root, rootAttrs, err := sniffRoot(b)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(root) {
	case "subtitlereel":
		doc.Format = FormatST428
	case "dcsubtitle":
		doc.Format = FormatInterop
		doc.TimeCodeRate = InteropTicksPerSecond
		doc.EditRate = EditRate{Num: InteropTicksPerSecond, Den: 1}
	default:
		return nil, fmt.Errorf("unrecognized timed-text root element %q", root)
	}
	// xml:lang on the root is a language fallback.
	for _, a := range rootAttrs {
		if strings.EqualFold(a.Name.Local, "lang") {
			doc.Language = a.Value
		}
	}

	// Second pass: stream tokens, capturing header fields and every Subtitle
	// element at any nesting depth (Subtitles are nested under Font elements).
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false       // tolerate quirky entities / namespaces in field content
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse timed text: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(se.Name.Local) {
		case "editrate":
			var v string
			if err := dec.DecodeElement(&v, &se); err == nil {
				if er, err := ParseEditRate(v); err == nil {
					doc.EditRate = er
				}
			}
		case "timecoderate":
			var v string
			if err := dec.DecodeElement(&v, &se); err == nil {
				var n float64
				if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &n); err == nil && n > 0 {
					doc.TimeCodeRate = n
				}
			}
		case "language":
			if doc.Language == "" {
				var v string
				if err := dec.DecodeElement(&v, &se); err == nil {
					doc.Language = strings.TrimSpace(v)
				}
			}
		case "subtitle":
			var st xmlSubtitle
			if err := dec.DecodeElement(&st, &se); err != nil {
				doc.Warnings = append(doc.Warnings, "subtitle_decode_error")
				continue
			}
			doc.appendSubtitle(st)
		}
	}

	// Resolve the conversion rate for SMPTE: prefer TimeCodeRate, else EditRate.
	if doc.Format == FormatST428 && doc.TimeCodeRate <= 0 {
		if doc.EditRate.Valid() {
			doc.TimeCodeRate = doc.EditRate.PerSecond()
		} else {
			doc.TimeCodeRate = 24 // safe default; recorded as a warning
			doc.Warnings = append(doc.Warnings, "missing_editrate_defaulted_24")
		}
	}

	// Re-resolve cue times now that the rate is known. (Subtitles may appear
	// before the rate elements in malformed files; we stored raw times.)
	doc.resolveTimes()
	return doc, nil
}

// xmlSubtitle matches a <Subtitle> element by local name regardless of namespace.
type xmlSubtitle struct {
	SpotNumber string   `xml:"SpotNumber,attr"`
	TimeIn     string   `xml:"TimeIn,attr"`
	TimeOut    string   `xml:"TimeOut,attr"`
	VPosition  string   `xml:"VPosition,attr"`
	HPosition  string   `xml:"HPosition,attr"`
	VAlign     string   `xml:"VAlign,attr"`
	HAlign     string   `xml:"HAlign,attr"`
	Texts      []xmlTextRun `xml:"Text"`
}

// xmlTextRun captures a <Text> line's inner XML so nested font runs / <br/> are
// flattened to plain text and newlines.
type xmlTextRun struct {
	Inner string `xml:",innerxml"`
}

// rawCue holds a cue with its still-textual times, resolved later once the rate is known.
type rawCue struct {
	cue     model.Cue
	timeIn  string
	timeOut string
}

func (d *Document) appendSubtitle(st xmlSubtitle) {
	lines := make([]string, 0, len(st.Texts))
	for _, t := range st.Texts {
		lines = append(lines, flattenInnerXML(t.Inner))
	}
	text := strings.Join(lines, "\n")
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" && st.TimeIn == "" && st.TimeOut == "" {
		return // empty placeholder
	}
	cue := model.Cue{
		Text:     text,
		Language: d.Language,
		Kind:     model.TrackUnknown, // stamped by the RPL/429-12 layer
	}
	if pos := positionMap(st); len(pos) > 0 {
		cue.Position = pos
	}
	d.raw = append(d.raw, rawCue{cue: cue, timeIn: st.TimeIn, timeOut: st.TimeOut})
}

// raw is the staging slice of cues prior to time resolution.
//
//nolint:unused // populated by appendSubtitle, drained by resolveTimes
func (d *Document) resolveTimes() {
	rate := d.TimeCodeRate
	for _, rc := range d.raw {
		startMS, err1 := TimecodeToMS(rc.timeIn, rate)
		endMS, err2 := TimecodeToMS(rc.timeOut, rate)
		if err1 != nil || err2 != nil {
			d.Warnings = append(d.Warnings, "cue_time_parse_error")
			continue
		}
		if endMS < startMS {
			d.Warnings = append(d.Warnings, "cue_end_before_start")
			continue
		}
		c := rc.cue
		c.StartMS = startMS
		c.EndMS = endMS
		d.Cues = append(d.Cues, c)
	}
	d.raw = nil
}

// positionMap captures opaque position attributes for pass-through (not rendered).
func positionMap(st xmlSubtitle) map[string]string {
	m := map[string]string{}
	add := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	add("vposition", st.VPosition)
	add("hposition", st.HPosition)
	add("valign", st.VAlign)
	add("halign", st.HAlign)
	if len(m) == 0 {
		return nil
	}
	return m
}

// flattenInnerXML strips markup from a <Text> element's inner XML, keeping
// character data and converting <br/> to a newline. This captures nested font-run
// text without interpreting styling (§5.4 defer-styling).
func flattenInnerXML(inner string) string {
	if inner == "" {
		return ""
	}
	dec := xml.NewDecoder(strings.NewReader(inner))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.AutoClose = xml.HTMLAutoClose
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.StartElement:
			if strings.EqualFold(t.Name.Local, "br") {
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			if strings.EqualFold(t.Name.Local, "br") {
				// self-closing handled at StartElement; nothing here
			}
		}
	}
	// Collapse runs of spaces/tabs but preserve newlines.
	return normalizeWhitespace(sb.String())
}

func normalizeWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		fields := strings.Fields(ln)
		lines[i] = strings.Join(fields, " ")
	}
	return strings.Join(lines, "\n")
}

// sniffRoot returns the local name and attributes of the first element.
func sniffRoot(b []byte) (string, []xml.Attr, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return "", nil, fmt.Errorf("no root element")
		}
		if err != nil {
			return "", nil, fmt.Errorf("sniff root: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, se.Attr, nil
		}
	}
}
