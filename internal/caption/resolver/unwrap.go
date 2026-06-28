package resolver

import (
	"bytes"
	"errors"
	"fmt"
)

// Unwrap converts a fetched resource into timed-text XML. The fetched bytes may
// be (a) plain timed-text XML, (b) a track-file reference, or (c) an MXF-wrapped
// timed-text track file (ST 429-5). This stage exists even when the first target
// server returns plain XML, so a track-file-returning server is a configuration
// case, not a rewrite (§5.3).
//
// PROVISIONAL: real ST 429-5 MXF demuxing belongs to asdcplib (present on this
// dev host at /home/libserv-2/asdcplib; see DECISIONS/BLOCKERS). Until that is
// wired in, MXF unwrap is a heuristic extraction of the contiguous timed-text XML
// essence, sufficient for the simulator's synthetic track files and clearly
// seamed for replacement.
func Unwrap(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty resource")
	}
	trimmed := bytes.TrimLeft(b, " \t\r\n\xef\xbb\xbf") // whitespace + UTF-8 BOM
	if len(trimmed) > 0 && trimmed[0] == '<' {
		return trimmed, nil // already plain timed-text XML
	}
	if isMXF(b) {
		return extractXMLFromMXF(b)
	}
	return nil, fmt.Errorf("unrecognized resource container (not XML, not MXF)")
}

// mxfPartitionPackKeyPrefix is the start of an MXF Partition Pack key
// (SMPTE 377 / 429). MXF files begin with a run-in or directly with this key.
var mxfPartitionPackKeyPrefix = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05}

func isMXF(b []byte) bool {
	// The partition pack key may appear after an optional run-in (<= 64 KiB).
	limit := len(b)
	if limit > 65536+16 {
		limit = 65536 + 16
	}
	return bytes.Contains(b[:limit], mxfPartitionPackKeyPrefix)
}

// xml root markers for the timed-text grammars we support.
var (
	xmlDeclMarker  = []byte("<?xml")
	smpteRootOpen  = []byte("<SubtitleReel")
	interopRootOpen = []byte("<DCSubtitle")
	smpteRootClose  = []byte("</SubtitleReel>")
	interopRootClose = []byte("</DCSubtitle>")
)

// extractXMLFromMXF heuristically extracts the contiguous timed-text XML payload
// embedded in an MXF Generic Container essence. It locates the XML start and the
// last matching root close tag.
func extractXMLFromMXF(b []byte) ([]byte, error) {
	start := firstIndex(b, xmlDeclMarker, smpteRootOpen, interopRootOpen)
	if start < 0 {
		return nil, errors.New("MXF: no embedded timed-text XML found (use asdcplib for full ST 429-5 demux)")
	}
	// Choose whichever close tag is present; take its last occurrence + length.
	end := -1
	if i := bytes.LastIndex(b, smpteRootClose); i >= 0 {
		end = i + len(smpteRootClose)
	} else if i := bytes.LastIndex(b, interopRootClose); i >= 0 {
		end = i + len(interopRootClose)
	}
	if end < 0 || end <= start {
		return nil, errors.New("MXF: timed-text XML root close tag not found")
	}
	out := make([]byte, end-start)
	copy(out, b[start:end])
	return out, nil
}

func firstIndex(b []byte, markers ...[]byte) int {
	best := -1
	for _, m := range markers {
		if i := bytes.Index(b, m); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}
