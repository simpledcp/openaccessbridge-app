package rpl

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/caption/timedtext"
)

// ParseBytes parses an ST 430-11 RPL document (PROVISIONAL grammar). It is
// namespace-tolerant and resilient: an empty resource list is preserved (not an
// error), and an unparseable resource is skipped rather than failing the whole RPL.
func ParseBytes(b []byte) (*RPL, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity

	r := &RPL{}
	var (
		sawRoot    bool
		curReel    *Reel
		inReel     bool
		reelNumBuf string
	)

	readText := func(se xml.StartElement) string {
		var s string
		_ = dec.DecodeElement(&s, &se)
		return strings.TrimSpace(s)
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse rpl: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			if ee, ok := tok.(xml.EndElement); ok && strings.EqualFold(ee.Name.Local, "reel") {
				if curReel != nil {
					if n, err := strconv.Atoi(reelNumBuf); err == nil {
						curReel.ReelNumber = n
					}
					r.Reels = append(r.Reels, *curReel)
				}
				curReel = nil
				inReel = false
				reelNumBuf = ""
			}
			continue
		}
		local := strings.ToLower(se.Name.Local)
		if !sawRoot {
			// Accept any root; the canonical local name is
			// AuxResourcePresentationList but vendors vary.
			sawRoot = true
			continue
		}
		switch local {
		case "playoutid", "playout_id":
			r.PlayoutID = readText(se)
		case "id":
			if !inReel && r.ID == "" {
				r.ID = readText(se)
			}
		case "reel":
			curReel = &Reel{}
			inReel = true
		case "reelnumber", "reel_number":
			if inReel {
				reelNumBuf = readText(se)
			}
		// Reel-level timing. A <Resource> is decoded whole (parseResource consumes
		// the element), so these can only be the reel's own children. They are what
		// keeps an uncaptioned reel — legitimately resource-less, §5.2 item 5 —
		// from collapsing to 0 ms and shifting every following reel (§5.3).
		// PROVISIONAL element names (D19 / Phase 0A): the real ST 430-11 spelling is
		// confirmed against a live server before it is relied on.
		case "duration":
			if inReel && curReel != nil {
				curReel.DurationUnits = atoiOr(readText(se), 0)
			}
		case "entrypoint", "entry_point":
			if inReel && curReel != nil {
				curReel.EntryPointUnits = atoiOr(readText(se), 0)
			}
		case "editrate", "edit_rate":
			if inReel && curReel != nil {
				if er, err := timedtext.ParseEditRate(readText(se)); err == nil {
					curReel.EditRate = er
				}
			}
		case "resource":
			res, err := parseResource(dec, se)
			if err != nil {
				// Skip a malformed resource; keep the rest (§5.3 tolerance).
				continue
			}
			if curReel == nil {
				curReel = &Reel{}
				inReel = true
			}
			curReel.Resources = append(curReel.Resources, res)
		}
	}
	if !sawRoot {
		return nil, fmt.Errorf("rpl: no root element")
	}
	return r, nil
}

// xmlResource matches a <Resource> by local name regardless of namespace.
type xmlResource struct {
	ID                string `xml:"Id"`
	Type              string `xml:"Type"`
	TrackType         string `xml:"TrackType"`
	Language          string `xml:"Language"`
	Locator           string `xml:"Locator"`
	Path              string `xml:"Path"`
	URI               string `xml:"URI"`
	AnnotationText    string `xml:"AnnotationText"`
	MimeType          string `xml:"MimeType"`
	EditRate          string `xml:"EditRate"`
	EntryPoint        string `xml:"EntryPoint"`
	IntrinsicDuration string `xml:"IntrinsicDuration"`
	Duration          string `xml:"Duration"`
}

func parseResource(dec *xml.Decoder, se xml.StartElement) (Resource, error) {
	var xr xmlResource
	if err := dec.DecodeElement(&xr, &se); err != nil {
		return Resource{}, err
	}
	typ := xr.Type
	if typ == "" {
		typ = xr.TrackType
	}
	res := Resource{
		ID:       strings.TrimSpace(xr.ID),
		RawType:  strings.TrimSpace(typ),
		Kind:     kindFromType(typ),
		Language: strings.TrimSpace(xr.Language),
		MimeType: strings.TrimSpace(xr.MimeType),
		Locator:  firstNonEmpty(xr.Locator, xr.Path, xr.URI, xr.AnnotationText),
	}
	if er, err := timedtext.ParseEditRate(xr.EditRate); err == nil {
		res.EditRate = er
	}
	res.EntryPointUnits = atoiOr(xr.EntryPoint, 0)
	dur := xr.IntrinsicDuration
	if dur == "" {
		dur = xr.Duration
	}
	res.IntrinsicDurationUnits = atoiOr(dur, 0)
	return res, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func atoiOr(s string, def int64) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return n
	}
	return def
}
