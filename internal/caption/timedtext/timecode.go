package timedtext

import (
	"fmt"
	"strconv"
	"strings"
)

// EditRate is a rational editable-unit rate (e.g. "24 1" → 24/1). In D-Cinema
// timed text the EditRate gives editable units per second.
type EditRate struct {
	Num int
	Den int
}

// PerSecond returns the editable units per second as a float.
func (e EditRate) PerSecond() float64 {
	if e.Den == 0 {
		return 0
	}
	return float64(e.Num) / float64(e.Den)
}

// Valid reports whether the edit rate is usable.
func (e EditRate) Valid() bool { return e.Num > 0 && e.Den > 0 }

// ParseEditRate parses a "NUM DEN" (or "NUM") edit-rate string.
func ParseEditRate(s string) (EditRate, error) {
	fields := strings.Fields(strings.TrimSpace(s))
	switch len(fields) {
	case 1:
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			return EditRate{}, fmt.Errorf("edit rate %q: %w", s, err)
		}
		return EditRate{Num: n, Den: 1}, nil
	case 2:
		n, err1 := strconv.Atoi(fields[0])
		d, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			return EditRate{}, fmt.Errorf("edit rate %q: invalid integers", s)
		}
		return EditRate{Num: n, Den: d}, nil
	default:
		return EditRate{}, fmt.Errorf("edit rate %q: want 1 or 2 fields", s)
	}
}

// InteropTicksPerSecond is the editable-unit rate used by legacy Interop
// (CineCanvas) subtitle XML: a tick is 1/250 of a second.
//
// PROVISIONAL: confirmed widely in field Interop content; reconcile against the
// licensed Interop subtitle spec (§5.4).
const InteropTicksPerSecond = 250

// TimecodeToMS converts a D-Cinema timed-text time expression to milliseconds.
//
// Supported forms (§5.4 "robust handling of edit-rate/timecode → milliseconds"):
//
//	HH:MM:SS:UU   — colon form; the 4th group is editable units (SMPTE frames or
//	                Interop ticks). unitsPerSecond selects the interpretation:
//	                SMPTE → TimeCodeRate (e.g. 24/25/30/48); Interop → 250.
//	HH:MM:SS.fff  — decimal-seconds form; fraction is taken directly.
//	HH:MM:SS      — whole seconds.
//
// unitsPerSecond must be > 0 for the colon form. The conversion rounds to the
// nearest millisecond. Negative results are rejected.
func TimecodeToMS(tc string, unitsPerSecond float64) (int64, error) {
	tc = strings.TrimSpace(tc)
	if tc == "" {
		return 0, fmt.Errorf("empty timecode")
	}

	// Decimal-seconds form: HH:MM:SS.fff
	if dot := strings.IndexByte(tc, '.'); dot >= 0 {
		head := tc[:dot]
		frac := tc[dot:] // includes leading '.'
		hh, mm, ss, err := splitHMS(head)
		if err != nil {
			return 0, err
		}
		secFrac, err := strconv.ParseFloat("0"+frac, 64)
		if err != nil {
			return 0, fmt.Errorf("timecode %q fraction: %w", tc, err)
		}
		total := (float64(hh*3600+mm*60+ss) + secFrac) * 1000.0
		return roundNonNeg(total, tc)
	}

	parts := strings.Split(tc, ":")
	switch len(parts) {
	case 3: // HH:MM:SS
		hh, mm, ss, err := splitHMSParts(parts)
		if err != nil {
			return 0, err
		}
		return int64(hh*3600+mm*60+ss) * 1000, nil
	case 4: // HH:MM:SS:UU
		if unitsPerSecond <= 0 {
			return 0, fmt.Errorf("timecode %q: editable-unit form requires unitsPerSecond > 0", tc)
		}
		hh, mm, ss, err := splitHMSParts(parts[:3])
		if err != nil {
			return 0, err
		}
		units, err := strconv.Atoi(parts[3])
		if err != nil {
			return 0, fmt.Errorf("timecode %q units: %w", tc, err)
		}
		if units < 0 {
			return 0, fmt.Errorf("timecode %q: negative units", tc)
		}
		total := float64(hh*3600+mm*60+ss)*1000.0 + float64(units)*1000.0/unitsPerSecond
		return roundNonNeg(total, tc)
	default:
		return 0, fmt.Errorf("timecode %q: unrecognized format", tc)
	}
}

func roundNonNeg(ms float64, tc string) (int64, error) {
	if ms < 0 {
		return 0, fmt.Errorf("timecode %q: negative result", tc)
	}
	// Round half away from zero.
	return int64(ms + 0.5), nil
}

func splitHMS(head string) (hh, mm, ss int, err error) {
	return splitHMSParts(strings.Split(head, ":"))
}

func splitHMSParts(parts []string) (hh, mm, ss int, err error) {
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("expected HH:MM:SS, got %q", strings.Join(parts, ":"))
	}
	if hh, err = strconv.Atoi(parts[0]); err != nil {
		return
	}
	if mm, err = strconv.Atoi(parts[1]); err != nil {
		return
	}
	if ss, err = strconv.Atoi(parts[2]); err != nil {
		return
	}
	if hh < 0 || mm < 0 || ss < 0 || mm > 59 || ss > 59 {
		return 0, 0, 0, fmt.Errorf("timecode field out of range in %02d:%02d:%02d", hh, mm, ss)
	}
	return hh, mm, ss, nil
}
