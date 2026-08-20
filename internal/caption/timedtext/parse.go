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
	FormatST428   Format = "smpte_st428" // ST 428-7 / 428-10 (SubtitleReel)
	FormatInterop Format = "interop"     // legacy CineCanvas (DCSubtitle)
	FormatUnknown Format = "unknown"
)

// Document is a parsed timed-text document with all cue times normalized to
// timeline-relative milliseconds.
type Document struct {
	Format       Format
	Language     string
	EditRate     EditRate
	TimeCodeRate float64 // editable units (frames/ticks) per second used for conversion
	Cues         []model.Cue
	// StartTimeMS is the document's <StartTime> origin in milliseconds — the
	// point on the reel/timeline the cue timecodes are authored against (§5.4).
	// Cues are ALREADY normalized against it; it is exposed for diagnostics, and
	// callers must NOT subtract it a second time.
	StartTimeMS int64
	// Warnings record non-fatal parse issues as classes/counts — never content.
	Warnings []string

	// raw stages cues before time resolution (rate may appear after subtitles in
	// malformed files); drained by resolveTimes.
	raw []rawCue
	// rawStartTime stages the <StartTime> text for the same reason.
	rawStartTime string
	// fonts is the stack of enclosing <Font> attribute frames whose opaque style
	// is carried into each cue (§5.4 defer-styling).
	fonts []map[string]string
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
	dec.Strict = false // tolerate quirky entities / namespaces in field content
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
		if ee, ok := tok.(xml.EndElement); ok {
			if strings.EqualFold(ee.Name.Local, "font") {
				doc.popFont()
			}
			continue
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
		case "starttime":
			// The reel/timeline origin the cue timecodes are authored against.
			// Staged as text: the rate it converts at may not be known yet (§5.4).
			var v string
			if err := dec.DecodeElement(&v, &se); err == nil {
				doc.rawStartTime = strings.TrimSpace(v)
			}
		case "language":
			if doc.Language == "" {
				var v string
				if err := dec.DecodeElement(&v, &se); err == nil {
					doc.Language = strings.TrimSpace(v)
				}
			}
		case "font":
			// <Font> encloses the subtitles it styles; its attributes ride along
			// as opaque pass-through on every cue inside it (§5.4).
			doc.pushFont(se.Attr)
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
// Everything except the timing attributes is captured verbatim: the grammars
// disagree on attribute spelling (Interop "VAlign" vs ST 428-7 "Valign"), and
// styling/positioning is opaque pass-through anyway (§5.4), so it is collected
// generically rather than field by field.
type xmlSubtitle struct {
	TimeIn  string       `xml:"TimeIn,attr"`
	TimeOut string       `xml:"TimeOut,attr"`
	Attrs   []xml.Attr   `xml:",any,attr"`
	Texts   []xmlTextRun `xml:"Text"`
}

// xmlTextRun captures a <Text> line's attributes plus its inner XML, so nested
// font runs / <br/> flatten to plain text and newlines. Both shipped grammars
// carry positioning on <Text>, not on <Subtitle>.
type xmlTextRun struct {
	Attrs []xml.Attr `xml:",any,attr"`
	Inner string     `xml:",innerxml"`
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

	// Opaque pass-through (§5.4): style accumulates outer→inner (enclosing
	// <Font>, then the <Subtitle>, then the first <Text> run); positioning is
	// declared per <Text> in both grammars, so the first run that carries any
	// wins — per-line positioning is deferred with the rest of the rendering.
	style := d.currentFontStyle()
	pos := map[string]string{}
	mergeAttrs(st.Attrs, style, pos)
	for _, t := range st.Texts {
		runPos := map[string]string{}
		mergeAttrs(t.Attrs, style, runPos)
		if len(runPos) > 0 {
			for k, v := range runPos {
				pos[k] = v
			}
			break
		}
	}
	if len(style) > 0 {
		cue.Style = style
	}
	if len(pos) > 0 {
		cue.Position = pos
	}
	d.raw = append(d.raw, rawCue{cue: cue, timeIn: st.TimeIn, timeOut: st.TimeOut})
}

// pushFont enters an enclosing <Font>, inheriting the frame around it so nested
// fonts merge outer→inner.
func (d *Document) pushFont(attrs []xml.Attr) {
	frame := map[string]string{}
	if n := len(d.fonts); n > 0 {
		for k, v := range d.fonts[n-1] {
			frame[k] = v
		}
	}
	mergeAttrs(attrs, frame, map[string]string{})
	d.fonts = append(d.fonts, frame)
}

// popFont leaves an enclosing <Font>. Tolerant of unbalanced markup (the decoder
// runs non-strict, so a stray close tag must not panic the parse).
func (d *Document) popFont() {
	if n := len(d.fonts); n > 0 {
		d.fonts = d.fonts[:n-1]
	}
}

// currentFontStyle copies the innermost enclosing <Font> frame.
func (d *Document) currentFontStyle() map[string]string {
	m := map[string]string{}
	if n := len(d.fonts); n > 0 {
		for k, v := range d.fonts[n-1] {
			m[k] = v
		}
	}
	return m
}

// resolveTimes drains the staged cues, converting their timecodes at the resolved
// rate and normalizing them against the document's <StartTime> origin so the
// result is a single timeline-relative start_ms/end_ms (§5.4). Assets are
// routinely authored on an hour-offset axis (01:00:00:00); without the
// subtraction every cue is an hour late and never displays.
//
//nolint:unused // populated by appendSubtitle, drained here
func (d *Document) resolveTimes() {
	rate := d.TimeCodeRate
	if d.rawStartTime != "" {
		if origin, err := TimecodeToMS(d.rawStartTime, rate); err == nil {
			d.StartTimeMS = origin
		} else {
			d.Warnings = append(d.Warnings, "starttime_parse_error")
		}
	}
	origin := d.StartTimeMS
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
		if origin > 0 {
			startMS -= origin
			endMS -= origin
			if endMS <= 0 {
				// Ends at or before the origin: never displayable. Drop it loudly
				// rather than emit a negative interval.
				d.Warnings = append(d.Warnings, "cue_before_start_time")
				continue
			}
			if startMS < 0 {
				// Straddles the origin: clamp the in-point, keep the cue.
				d.Warnings = append(d.Warnings, "cue_clamped_to_start_time")
				startMS = 0
			}
		}
		c := rc.cue
		c.StartMS = startMS
		c.EndMS = endMS
		d.Cues = append(d.Cues, c)
	}
	d.raw = nil
	d.rawStartTime = ""
	d.fonts = nil
}

// positionAttrs are the opaque positioning attributes split out of the style map
// into Cue.Position. Both are carried, neither is rendered (§5.4).
var positionAttrs = map[string]bool{
	"vposition": true,
	"hposition": true,
	"zposition": true,
	"valign":    true,
	"halign":    true,
}

// mergeAttrs sorts opaque attributes into the position and style maps, keyed by
// lowercased local name so the Interop and ST 428-7 spellings agree. Namespace
// declarations and the SpotNumber identifier are not styling and are skipped;
// TimeIn/TimeOut never reach here (they are decoded into their own fields).
func mergeAttrs(attrs []xml.Attr, style, pos map[string]string) {
	for _, a := range attrs {
		if a.Value == "" {
			continue
		}
		if a.Name.Space == "xmlns" || strings.EqualFold(a.Name.Local, "xmlns") {
			continue
		}
		k := strings.ToLower(a.Name.Local)
		if k == "spotnumber" {
			continue
		}
		if positionAttrs[k] {
			pos[k] = a.Value
			continue
		}
		style[k] = a.Value
	}
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
