package sim

import (
	"sort"
	"strconv"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/acsp"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Scenario is one named, self-contained ACSP behavior the simulator reproduces.
// Run drives the harness and asserts the expected caption-path outcome.
type Scenario struct {
	Name string
	Desc string
	Opts HarnessOptions
	Run  func(t T, h *Harness)
}

// singleReelShow builds a one-reel show with caption tracks per language and
// returns the media-block files plus the RPL bytes. Resources are emitted in
// sorted language order for determinism.
func singleReelShow(playout string, durationMS int64, langCues map[string][]CueSpec) (map[string][]byte, []byte) {
	files := map[string][]byte{}
	langs := make([]string, 0, len(langCues))
	for l := range langCues {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	var res []ResSpec
	for _, lang := range langs {
		// Namespace locators/IDs by playout so two shows in one scenario (e.g.
		// show-change) don't collide on a shared key.
		loc := "tt_" + playout + "_" + lang + ".xml"
		files[loc] = TimedText(lang, langCues[lang])
		res = append(res, ResSpec{ID: "res-" + playout + "-" + lang, Type: "ClosedCaption", Language: lang, Locator: loc})
	}
	return files, RPL(playout, []ReelSpec{{Number: 1, DurationMS: durationMS, Resources: res}})
}

// segmentShow builds a one-reel RPL for a single timeline segment, with file names
// of its own so two segments can share a Playout ID — the §5.2 item 4 quirk —
// without their assets colliding.
func segmentShow(name, playout string, durationMS int64, cues []CueSpec) (map[string][]byte, []byte) {
	loc := "tt_" + name + "_en.xml"
	files := map[string][]byte{loc: TimedText("en", cues)}
	return files, RPL(playout, []ReelSpec{{Number: 1, DurationMS: durationMS,
		Resources: []ResSpec{{ID: "res-" + name, Type: "ClosedCaption", Language: "en", Locator: loc}}}})
}

// reelLocator is the media-block locator of one reel's en caption asset in a show
// built by multiReelShow.
func reelLocator(playout string, reel int) string {
	return "tt_" + playout + "_reel" + strconv.Itoa(reel) + "_en.xml"
}

// multiReelShow builds an N-reel show of equal-length reels. An empty text means an
// UNCAPTIONED reel: no resources at all, which is legitimate (§5.2 item 5) and
// anchors only because the reel declares its own duration (§5.3).
func multiReelShow(playout string, reelMS int64, texts []string) (map[string][]byte, []byte) {
	files := map[string][]byte{}
	var reels []ReelSpec
	for i, text := range texts {
		spec := ReelSpec{Number: i + 1, DurationMS: reelMS}
		if text != "" {
			loc := reelLocator(playout, i+1)
			files[loc] = TimedText("en", []CueSpec{{StartMS: 500, EndMS: reelMS - 500, Text: text}})
			spec.Resources = []ResSpec{{
				ID: "res-" + playout + "-r" + strconv.Itoa(i+1), Type: "ClosedCaption",
				Language: "en", Locator: loc,
			}}
		}
		reels = append(reels, spec)
	}
	return files, RPL(playout, reels)
}

// connect runs the canonical new-connection sequence (§5.2): Announce → Get New
// Lease → Update Timeline → Set RPL Location → Set Output Mode. Note the RPL
// (which loads cues) arrives AFTER Update Timeline, exercising the mid-show engine
// seeding.
func connect(h *Harness, leaseMS, timelineMS int64, rpl []byte, outputEnabled bool) {
	h.Machine.Connected()
	h.Send(Announce("srv-1", "Sim Auditorium", ""))
	h.Send(GetNewLease(leaseMS))
	h.Send(UpdateTimeline(timelineMS, "1", acsp.PlayPlaying))
	h.Send(SetRPLInline(rpl))
	h.Send(SetOutputMode(outputEnabled))
}

// All returns the full quirk scenario suite — the caption-path regression backbone
// run headless in CI (§11.1).
func All() []Scenario {
	return []Scenario{
		happyPath(),
		newConnectionMidPlayback(),
		leaseActualExpiry(),
		leaseNearExpiryThenRenew(),
		commsLossContinueUntilLease(),
		commsLossThenLeaseExpiry(),
		playoutIDZeroNoLoopback(),
		duplicatePlayoutID(),
		emptyReelResources(),
		outputModeToggle(),
		outputModeAbsent(),
		showChangeNewShow(),
		rewind(),
		pauseResumeStop(),
		multiLanguage(),
		malformedAssetDegrades(),
		wrongRoomFingerprint(),
		multiReelAdvance(),
		midShowReconnectSkipsPastReels(),
		uncaptionedReelBetweenCaptioned(),
		twoRPLTimelineSegments(),
		twoRPLSegmentsZeroPlayout(),
		abandonedRPLNewShow(),
		showChangeReusedResourceID(),
		leaseExpirySelfHeal(),
		reconnectAfterLeaseExpiry(),
		unfetchableAsset(),
		timelineCadence(),
		statusCarriesTimeline(),
		languageOnDemandFetch(),
	}
}

func happyPath() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 1000, EndMS: 3000, Text: "Hello"}, {StartMS: 5000, EndMS: 7000, Text: "World"}},
	})
	return Scenario{
		Name: "happy-path",
		Desc: "Canonical new-connection sequence renders cues across boundaries.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AssertState(t, model.StatePlaying)
			h.AssertNoCaption(t, "en") // pos 0, first cue at 1000
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Hello")
			h.AdvanceEval(2000 * time.Millisecond) // pos 3000 → cleared
			h.AssertNoCaption(t, "en")
			h.AdvanceEval(2000 * time.Millisecond) // pos 5000 → World
			h.AssertCaption(t, "en", "World")
		},
	}
}

func newConnectionMidPlayback() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 1000, EndMS: 9000, Text: "Mid-show cue"}},
	})
	return Scenario{
		Name: "new-connection-mid-playback",
		Desc: "Connection arriving during playback shows the already-active cue at once.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 4000, rpl, true) // join at timeline 4000, within the cue
			h.AssertState(t, model.StatePlaying)
			// The cue [1000,9000) is active at 4000 and must show immediately,
			// even though the RPL arrived after Update Timeline.
			h.AssertCaption(t, "en", "Mid-show cue")
		},
	}
}

func leaseActualExpiry() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Holding"}},
	})
	return Scenario{
		Name: "lease-actual-expiry",
		Desc: "When the lease genuinely expires with no renewal, captions blank.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 10000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Holding")
			// No status/renewal; advance past the lease.
			h.AdvanceTick(9000 * time.Millisecond) // total 11s > 10s lease
			h.AssertState(t, model.StateLeaseExpired)
			h.AssertNoCaption(t, "en")
		},
	}
}

func leaseNearExpiryThenRenew() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Holding"}},
	})
	return Scenario{
		Name: "lease-near-expiry-then-renew",
		Desc: "A status arriving close to the lease raises an operator warning, then renews it (§5.2 #1).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 10000, 0, rpl, true)
			h.AdvanceTick(8000 * time.Millisecond) // near expiry (warning territory)
			h.AssertState(t, model.StatePlaying)
			// "Treat anything approaching the lease as a warning" — loudly, not in
			// a log line only.
			if !h.HasWarning(acsp.WarnLeaseNearExpiry) {
				t.Errorf("expected a lease-near-expiry warning at 80%% of the lease")
			}
			h.Send(Status(10000)) // renew at half-lease-ish cadence
			h.AdvanceTick(5000 * time.Millisecond)
			h.AssertState(t, model.StatePlaying)
			h.AssertCaption(t, "en", "Holding")
		},
	}
}

func commsLossContinueUntilLease() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Holding"}},
	})
	return Scenario{
		Name: "comms-loss-continue-until-lease",
		Desc: "A momentary comms interruption keeps captions showing until the lease expires (§5.2 #2).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 20000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Holding")
			// Go silent past the comms threshold (lease/2 + lease/10 = 12s) but
			// before the 20s lease expiry.
			h.AdvanceTick(13000 * time.Millisecond)
			h.AssertCommsLost(t, true)
			h.AssertCaption(t, "en", "Holding") // STILL showing
			// A status arrives before lease expiry → restored.
			h.Send(Status(20000))
			h.AssertCommsLost(t, false)
			h.AssertCaption(t, "en", "Holding")
		},
	}
}

func commsLossThenLeaseExpiry() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Holding"}},
	})
	return Scenario{
		Name: "comms-loss-then-lease-expiry",
		Desc: "Sustained comms loss past the lease finally blanks captions.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 15000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Holding")
			h.AdvanceTick(10000 * time.Millisecond) // comms loss (>8.25s threshold)
			h.AssertCommsLost(t, true)
			h.AssertCaption(t, "en", "Holding")
			h.AdvanceTick(6000 * time.Millisecond) // total 17s > 15s lease → expire
			h.AssertState(t, model.StateLeaseExpired)
			h.AssertNoCaption(t, "en")
		},
	}
}

func playoutIDZeroNoLoopback() Scenario {
	files, rpl := singleReelShow("0", 60000, map[string][]CueSpec{
		"en": {{StartMS: 1000, EndMS: 3000, Text: "First"}, {StartMS: 5000, EndMS: 7000, Text: "Second"}},
	})
	return Scenario{
		Name: "playout-id-zero-no-loopback",
		Desc: "Repeated Playout ID = 0 with an advancing timeline is not a seek-to-start (§5.2 #4).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "First")
			// Server keeps sending Playout ID = 0 with advancing timeline.
			h.Send(UpdateTimeline(5000, "0", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Second")
			// Must NOT have looped back to "First".
			if got := h.State().TimelinePositionMS; got != 5000 {
				t.Errorf("timeline = %d, want 5000 (no loopback)", got)
			}
		},
	}
}

func duplicatePlayoutID() Scenario {
	files, rpl := singleReelShow("7", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Same playout"}},
	})
	return Scenario{
		Name: "duplicate-playout-id",
		Desc: "A duplicate Playout ID with advancing timeline does not reset playback.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.Send(UpdateTimeline(10000, "7", acsp.PlayPlaying)) // same playout id, later time
			h.Evaluate()
			h.AssertCaption(t, "en", "Same playout")
			if got := h.State().TimelinePositionMS; got != 10000 {
				t.Errorf("timeline = %d, want 10000", got)
			}
		},
	}
}

func emptyReelResources() Scenario {
	rpl := RPL("3", []ReelSpec{{Number: 1, DurationMS: 60000, Resources: nil}}) // trailer: no captions
	return Scenario{
		Name: "empty-reel-resources",
		Desc: "A reel with no caption resources renders 'no captions for this segment', not an error (§5.2 #5).",
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertNoCaption(t, "en")
			// The link is still healthy/playing; just no captions for this content.
			if h.State().State == model.StateError {
				t.Errorf("empty resources must not be an error state")
			}
		},
	}
}

func outputModeToggle() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Captioned"}},
	})
	return Scenario{
		Name: "output-mode-toggle",
		Desc: "Set Output Mode disabled suppresses caption OUTPUT, not just the status label (§5.2 #3).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Captioned")

			h.Send(SetOutputMode(false))
			if got := h.PatronStatus("en"); got != string(model.PatronCaptionNoCaptions) {
				t.Errorf("disabled output status = %q, want no_captions", got)
			}
			// The text itself must stop going out: the server said this content is
			// not captioned, and a phone that still shows a cue contradicts it.
			h.AssertNoCaption(t, "en")
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertNoCaption(t, "en") // and stays suppressed as the timeline runs

			h.Send(SetOutputMode(true))
			if got := h.PatronStatus("en"); got != string(model.PatronCaptionActive) {
				t.Errorf("re-enabled output status = %q, want active", got)
			}
			h.Evaluate()
			h.AssertCaption(t, "en", "Captioned") // and the cue comes back
		},
	}
}

func outputModeAbsent() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Captioned"}},
	})
	return Scenario{
		Name: "output-mode-absent",
		Desc: "A server that never sends Set Output Mode is handled gracefully (assume unknown, still render).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			// Connect WITHOUT Set Output Mode.
			h.Machine.Connected()
			h.Send(Announce("srv-1", "Sim Auditorium", ""))
			h.Send(GetNewLease(30000))
			h.Send(UpdateTimeline(0, "1", acsp.PlayPlaying))
			h.Send(SetRPLInline(rpl))
			h.AdvanceEval(1000 * time.Millisecond)
			if got := h.State().OutputMode; got != model.OutputUnknown {
				t.Errorf("output mode = %q, want unknown", got)
			}
			h.AssertCaption(t, "en", "Captioned") // not suppressed on unknown
		},
	}
}

func showChangeNewShow() Scenario {
	files1, rpl1 := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Feature A"}},
	})
	files2, rpl2 := singleReelShow("2", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Feature B"}},
	})
	files := mergeFiles(files1, files2)
	return Scenario{
		Name: "show-change-new-show",
		Desc: "Terminate Lease drops the old show; the new-show sequence loads new content (§5.2 new-show-load).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl1, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Feature A")
			// New show: Terminate → GetNewLease → UpdateTimeline → SetRPL.
			h.Send(TerminateLease())
			h.AssertNoCaption(t, "en") // old content dropped
			h.AssertNoCachedContent(t) // including the fetched timed text itself
			h.Send(GetNewLease(30000))
			h.Send(UpdateTimeline(0, "1", acsp.PlayPlaying))
			h.Send(SetRPLInline(rpl2))
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Feature B")
		},
	}
}

func rewind() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 1000, EndMS: 3000, Text: "Early"}, {StartMS: 20000, EndMS: 22000, Text: "Late"}},
	})
	return Scenario{
		Name: "rewind",
		Desc: "A backward Update Timeline (rewind) re-anchors and re-resolves cues at the earlier position.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(20500 * time.Millisecond)
			h.AssertCaption(t, "en", "Late")
			h.Send(UpdateTimeline(1500, "1", acsp.PlayPlaying)) // rewind to 1500
			h.Evaluate()
			h.AssertCaption(t, "en", "Early")
		},
	}
}

func pauseResumeStop() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 1000, EndMS: 30000, Text: "Long cue"}},
	})
	return Scenario{
		Name: "pause-resume-stop",
		Desc: "Pause freezes the cue, resume continues, stop blanks.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 60000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Long cue")
			h.Send(UpdateTimeline(2000, "1", acsp.PlayPaused))
			h.AdvanceEval(100000 * time.Millisecond) // long wall time while paused
			h.AssertCaption(t, "en", "Long cue")     // frozen, still shown
			h.Send(UpdateTimeline(2000, "1", acsp.PlayPlaying))
			h.AssertCaption(t, "en", "Long cue")
			h.Send(UpdateTimeline(2000, "1", acsp.PlayStopped))
			h.AssertNoCaption(t, "en")
		},
	}
}

func multiLanguage() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en":     {{StartMS: 1000, EndMS: 5000, Text: "Hello"}},
		"es-419": {{StartMS: 1000, EndMS: 5000, Text: "Hola"}},
	})
	return Scenario{
		Name: "multi-language",
		Desc: "Two caption languages render independently from one timeline; preferred order honored.",
		Opts: HarnessOptions{Files: files, PreferredLanguages: []string{"es-419", "en"}},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Hello")
			h.AssertCaption(t, "es-419", "Hola")
			// Preferred language is first in the link state.
			if got := h.State().CurrentLanguage; got != "es-419" {
				t.Errorf("current language = %q, want es-419 (preferred)", got)
			}
		},
	}
}

func malformedAssetDegrades() Scenario {
	// en resource points at valid timed text; es points at a garbage asset.
	enTT := TimedText("en", []CueSpec{{StartMS: 1000, EndMS: 5000, Text: "Hello"}})
	files := map[string][]byte{
		"tt_en.xml": enTT,
		"tt_es.xml": []byte("\x00\x01 not xml not mxf \xff"),
	}
	rpl := RPL("1", []ReelSpec{{Number: 1, DurationMS: 60000, Resources: []ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_en.xml"},
		{ID: "res-es", Type: "ClosedCaption", Language: "es", Locator: "tt_es.xml"},
	}}})
	return Scenario{
		Name: "malformed-asset-degrades",
		Desc: "A bad asset for one language degrades to 'no caption' for that language without crashing the others (§5.4).",
		Opts: HarnessOptions{Files: files, PreferredLanguages: []string{"en", "es"}},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Hello") // good language unaffected
			h.AssertNoCaption(t, "es")        // bad language degrades silently
		},
	}
}

func wrongRoomFingerprint() Scenario {
	files, _ := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 60000, Text: "Captioned"}},
	})
	return Scenario{
		Name: "wrong-room-fingerprint",
		Desc: "A server whose fingerprint does not match the pinned auditorium identity raises a loud warning (§8.5).",
		Opts: HarnessOptions{Files: files, ExpectedFingerprint: "EXPECTED-FP"},
		Run: func(t T, h *Harness) {
			h.Machine.Connected()
			h.Send(Announce("srv-evil", "Wrong Room", "DIFFERENT-FP"))
			if !h.HasWarning(model.WarnServerIdentityMismatch) {
				t.Errorf("expected a server-identity-mismatch warning")
			}
		},
	}
}

func multiReelAdvance() Scenario {
	files, rpl := multiReelShow("1", 60000, []string{"Reel One", "Reel Two", "Reel Three"})
	return Scenario{
		Name: "multi-reel-advance",
		Desc: "The fetch-ahead window follows the Update Timeline position across reel boundaries (§5.3).",
		Opts: HarnessOptions{Files: files, FetchAheadReels: 1},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Reel One")
			// The window is current+next: reel 3 must not have been pulled yet.
			h.AssertFetchCount(t, reelLocator("1", 3), 0)

			h.Send(UpdateTimeline(62000, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Reel Two") // was blank: loadReels(0) never advanced

			h.Send(UpdateTimeline(125000, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Reel Three")
		},
	}
}

func midShowReconnectSkipsPastReels() Scenario {
	files, rpl := multiReelShow("1", 60000, []string{"Reel One", "Reel Two", "Reel Three"})
	return Scenario{
		Name: "mid-show-reconnect-skips-past-reels",
		Desc: "A connection arriving during reel 3 honors the timeline and never fetches reels 1-2 (§5.3).",
		Opts: HarnessOptions{Files: files, FetchAheadReels: 1},
		Run: func(t T, h *Harness) {
			h.Machine.Connected()
			h.Send(Announce("srv-1", "Sim Auditorium", ""))
			h.Send(GetNewLease(30000))
			h.Send(UpdateTimeline(125000, "1", acsp.PlayPlaying)) // joining mid-show
			h.Send(SetRPLInline(rpl))
			h.Send(SetOutputMode(true))
			h.Evaluate()
			h.AssertCaption(t, "en", "Reel Three")
			// Content that will never play must not be pulled from the media block.
			h.AssertFetchCount(t, reelLocator("1", 1), 0)
			h.AssertFetchCount(t, reelLocator("1", 2), 0)
		},
	}
}

func uncaptionedReelBetweenCaptioned() Scenario {
	files, rpl := multiReelShow("1", 60000, []string{"Reel One", "", "Reel Three"})
	return Scenario{
		Name: "uncaptioned-reel-between-captioned-reels",
		Desc: "A resource-less middle reel still consumes its 60s of timeline, so reel 3 anchors correctly (§5.2 #5).",
		Opts: HarnessOptions{Files: files, FetchAheadReels: 1},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Reel One")

			h.Send(UpdateTimeline(65000, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertNoCaption(t, "en") // no captions for this segment, not an error
			if h.State().State == model.StateError {
				t.Errorf("an uncaptioned reel must not be an error state")
			}

			h.Send(UpdateTimeline(125000, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Reel Three")
		},
	}
}

func twoRPLTimelineSegments() Scenario {
	trailerFiles, trailerRPL := segmentShow("trailer", "0", 60000,
		[]CueSpec{{StartMS: 500, EndMS: 59500, Text: "Trailer"}})
	featureFiles, featureRPL := segmentShow("feature", "1", 120000,
		[]CueSpec{{StartMS: 500, EndMS: 119500, Text: "Feature"}})
	return Scenario{
		Name: "two-rpl-timeline-segments",
		Desc: "Two Set RPL Locations key by timeline segment: the feature must not render during the trailer (§5.3).",
		Opts: HarnessOptions{Files: mergeFiles(trailerFiles, featureFiles)},
		Run: func(t T, h *Harness) {
			h.Machine.Connected()
			h.Send(Announce("srv-1", "Sim Auditorium", ""))
			h.Send(GetNewLease(30000))
			h.Send(UpdateTimeline(0, "0", acsp.PlayPlaying))
			h.Send(SetRPLInline(trailerRPL))
			h.Send(SetRPLInline(featureRPL)) // the server names both upcoming segments
			h.Send(SetOutputMode(true))

			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Trailer") // "latest wins" rendered "Feature" here

			h.Send(UpdateTimeline(61000, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Feature")

			// Back into the trailer segment: still keyed by position, not by arrival.
			h.Send(UpdateTimeline(1000, "0", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Trailer")
		},
	}
}

func twoRPLSegmentsZeroPlayout() Scenario {
	// The quirk in full: a server that stamps Playout ID = 0 on every uncaptioned
	// composition gives two segments the SAME id, so only position can tell them
	// apart (§5.2 item 4).
	firstFiles, firstRPL := segmentShow("promo", "0", 60000,
		[]CueSpec{{StartMS: 500, EndMS: 59500, Text: "Promo"}})
	secondFiles, secondRPL := segmentShow("policy", "0", 60000,
		[]CueSpec{{StartMS: 500, EndMS: 59500, Text: "Policy"}})
	return Scenario{
		Name: "two-rpl-segments-duplicate-playout-id",
		Desc: "Two RPLs sharing Playout ID 0 are disambiguated by timeline position, never by the id (§5.2 #4).",
		Opts: HarnessOptions{Files: mergeFiles(firstFiles, secondFiles)},
		Run: func(t T, h *Harness) {
			h.Machine.Connected()
			h.Send(Announce("srv-1", "Sim Auditorium", ""))
			h.Send(GetNewLease(30000))
			h.Send(UpdateTimeline(0, "0", acsp.PlayPlaying))
			h.Send(SetRPLInline(firstRPL))
			h.Send(SetRPLInline(secondRPL))
			h.Send(SetOutputMode(true))

			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Promo")

			h.Send(UpdateTimeline(61000, "0", acsp.PlayPlaying)) // same id, later time
			h.Evaluate()
			h.AssertCaption(t, "en", "Policy")
			if got := h.State().TimelinePositionMS; got != 61000 {
				t.Errorf("timeline = %d, want 61000 (no loopback)", got)
			}
		},
	}
}

func abandonedRPLNewShow() Scenario {
	aFiles, aRPL := segmentShow("showa", "1", 60000,
		[]CueSpec{{StartMS: 500, EndMS: 59500, Text: "Feature A"}})
	bFiles, bRPL := segmentShow("showb", "2", 60000,
		[]CueSpec{{StartMS: 500, EndMS: 59500, Text: "Feature B"}})
	return Scenario{
		Name: "abandoned-rpl-new-show",
		Desc: "A Set RPL Location with no Terminate Lease must not leave the ACS holding the abandoned show (§5.2 new-show-load).",
		Opts: HarnessOptions{Files: mergeFiles(aFiles, bFiles)},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, aRPL, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Feature A")

			// No Terminate Lease: the server just names another RPL.
			h.Send(SetRPLInline(bRPL))
			h.Evaluate()
			h.AssertCaption(t, "en", "Feature A") // the segment now playing is unchanged

			h.Send(UpdateTimeline(61000, "2", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Feature B")
			if got := h.Machine.RetainedRPLCount(); got > 2 {
				t.Errorf("retained RPL segments = %d, want the set to stay bounded", got)
			}
			// The abandoned show's timed text was evicted at the boundary: coming
			// back for it costs a fresh fetch.
			h.AssertFetchCount(t, "tt_showa_en.xml", 1)
			h.Send(UpdateTimeline(1000, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Feature A")
			h.AssertFetchCount(t, "tt_showa_en.xml", 2)
		},
	}
}

func showChangeReusedResourceID() Scenario {
	// Both shows use the SAME resource ID with different locators — servers do reuse
	// ids across compositions, and a cache keyed by id alone served show A's text
	// for show B.
	rplFor := func(loc string) []byte {
		return RPL("9", []ReelSpec{{Number: 1, DurationMS: 60000, Resources: []ResSpec{
			{ID: "res-shared", Type: "ClosedCaption", Language: "en", Locator: loc},
		}}})
	}
	files := map[string][]byte{
		"tt_showa.xml": TimedText("en", []CueSpec{{StartMS: 0, EndMS: 59500, Text: "Feature A"}}),
		"tt_showb.xml": TimedText("en", []CueSpec{{StartMS: 0, EndMS: 59500, Text: "Feature B"}}),
	}
	return Scenario{
		Name: "show-change-reused-resource-id",
		Desc: "A new show reusing a resource ID gets its own content, and the old show's cache is purged (§5.3, guardrail #5).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rplFor("tt_showa.xml"), true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Feature A")

			h.Send(TerminateLease())
			h.AssertNoCaption(t, "en")
			h.AssertNoCachedContent(t) // guardrail #5: purged at show end

			h.Send(GetNewLease(30000))
			h.Send(UpdateTimeline(0, "9", acsp.PlayPlaying))
			h.Send(SetRPLInline(rplFor("tt_showb.xml")))
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Feature B") // was "Feature A" from the stale cache
		},
	}
}

func leaseExpirySelfHeal() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 59500, Text: "Holding"}},
	})
	return Scenario{
		Name: "lease-expiry-self-heal",
		Desc: "After a lease expiry a status that still carries the lease re-arms it instead of sitting dead (§5.2 #6).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 10000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Holding")

			h.AdvanceTick(11000 * time.Millisecond)
			h.AssertState(t, model.StateLeaseExpired)
			h.AssertNoCaption(t, "en")
			h.AssertNoCachedContent(t) // the expired show's content is not held

			// The server is still there: its next status re-arms the lease and the
			// sequence resumes.
			h.Send(Status(10000))
			if !h.State().LeaseActive {
				t.Errorf("a post-expiry status must re-arm the lease, not be ignored")
			}
			h.AssertState(t, model.StateLeased)
			h.Send(UpdateTimeline(20000, "1", acsp.PlayPlaying))
			h.Send(SetRPLInline(rpl))
			h.Evaluate()
			h.AssertState(t, model.StatePlaying)
			h.AssertCaption(t, "en", "Holding")
		},
	}
}

func reconnectAfterLeaseExpiry() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 59500, Text: "Holding"}},
	})
	return Scenario{
		Name: "reconnect-after-lease-expiry",
		Desc: "The full new-connection sequence after a lease expiry restores captions (§11.1 reconnect after expiry).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 10000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Holding")
			h.AdvanceTick(11000 * time.Millisecond)
			h.AssertState(t, model.StateLeaseExpired)
			h.AssertNoCaption(t, "en")

			// Re-run the whole sequence, as a re-dial would.
			connect(h, 30000, 5000, rpl, true)
			h.Evaluate()
			h.AssertState(t, model.StatePlaying)
			h.AssertCaption(t, "en", "Holding")
			if !h.State().LeaseActive {
				t.Errorf("expected a live lease after reconnecting")
			}
		},
	}
}

func unfetchableAsset() Scenario {
	// en resolves; es points at a locator the media block does not have at all.
	files := map[string][]byte{
		"tt_en.xml": TimedText("en", []CueSpec{{StartMS: 1000, EndMS: 5000, Text: "Hello"}}),
	}
	rpl := RPL("1", []ReelSpec{{Number: 1, DurationMS: 60000, Resources: []ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_en.xml"},
		{ID: "res-es", Type: "ClosedCaption", Language: "es", Locator: "tt_missing_es.xml"},
	}}})
	return Scenario{
		Name: "unfetchable-asset",
		Desc: "A resource the media block cannot serve degrades that language only, without an error state (§11.1).",
		Opts: HarnessOptions{Files: files, PreferredLanguages: []string{"en", "es"}},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Hello")
			h.AssertNoCaption(t, "es")
			h.AssertFetchCount(t, "tt_missing_es.xml", 1) // attempted, not silently skipped
			if h.State().State == model.StateError {
				t.Errorf("an unfetchable asset must not take down the caption link")
			}
		},
	}
}

func timelineCadence() Scenario {
	files, rpl := singleReelShow("1", 300000, map[string][]CueSpec{
		"en": {{StartMS: 0, EndMS: 10000, Text: "One"}, {StartMS: 10000, EndMS: 30000, Text: "Two"}},
	})
	return Scenario{
		Name: "timeline-cadence",
		Desc: "Timeline updates at a 1s cadence keep re-anchoring without drift (§11.1 configurable cadence).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 20000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "One")

			h.RunTimeline(1000*time.Millisecond, 15000, "1")
			h.AssertCaption(t, "en", "Two")
			h.AssertState(t, model.StatePlaying)
			st := h.State()
			if st.TimelinePositionMS < 15000 {
				t.Errorf("timeline = %d, want at least 15000", st.TimelinePositionMS)
			}
			if !st.LeaseActive {
				t.Errorf("the half-lease keepalive cadence must keep the lease alive (§5.2 #1)")
			}
			if st.DriftMSEstimate > 100 || st.DriftMSEstimate < -100 {
				t.Errorf("drift %d ms across the cadence run, want within 100ms", st.DriftMSEstimate)
			}
		},
	}
}

func statusCarriesTimeline() Scenario {
	files, rpl := singleReelShow("1", 60000, map[string][]CueSpec{
		"en": {{StartMS: 1000, EndMS: 3000, Text: "Early"}, {StartMS: 20000, EndMS: 22000, Text: "Late"}},
	})
	return Scenario{
		Name: "status-carries-timeline",
		Desc: "A periodic status carrying timeline state re-anchors; one carrying none stays a pure keepalive (§5.2).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(1500 * time.Millisecond)
			h.AssertCaption(t, "en", "Early")

			// Timeline arrives only on the status request.
			h.Send(StatusAt(30000, 20500, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Late")
			if got := h.State().TimelinePositionMS; got != 20500 {
				t.Errorf("timeline = %d, want 20500 from the status request", got)
			}

			// A status with no timeline must not re-anchor to zero.
			h.Send(Status(30000))
			h.Evaluate()
			if got := h.State().TimelinePositionMS; got != 20500 {
				t.Errorf("a bare keepalive moved the timeline to %d", got)
			}
			h.AssertCaption(t, "en", "Late")
		},
	}
}

func languageOnDemandFetch() Scenario {
	// Two reels, two languages, so the on-demand language can be followed across a
	// reel boundary.
	files := map[string][]byte{}
	var reels []ReelSpec
	for _, r := range []struct {
		n       int
		en, es  string
		enLoc   string
		esLoc   string
		cueSpec []CueSpec
	}{
		{n: 1, en: "Hello 1", es: "Hola 1", enLoc: "tt_r1_en.xml", esLoc: "tt_r1_es.xml"},
		{n: 2, en: "Hello 2", es: "Hola 2", enLoc: "tt_r2_en.xml", esLoc: "tt_r2_es.xml"},
	} {
		files[r.enLoc] = TimedText("en", []CueSpec{{StartMS: 500, EndMS: 59500, Text: r.en}})
		files[r.esLoc] = TimedText("es", []CueSpec{{StartMS: 500, EndMS: 59500, Text: r.es}})
		reels = append(reels, ReelSpec{Number: r.n, DurationMS: 60000, Resources: []ResSpec{
			{ID: "res-en-r" + strconv.Itoa(r.n), Type: "ClosedCaption", Language: "en", Locator: r.enLoc},
			{ID: "res-es-r" + strconv.Itoa(r.n), Type: "ClosedCaption", Language: "es", Locator: r.esLoc},
		}})
	}
	rpl := RPL("1", reels)
	return Scenario{
		Name: "language-on-demand-fetch",
		Desc: "With a capped eager fetch, a patron switching to an unfetched language loads it without disturbing the others (§5.3).",
		Opts: HarnessOptions{Files: files, PreferredLanguages: []string{"en", "es"},
			EagerLanguages: 1, FetchAheadReels: 0},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(2000 * time.Millisecond)
			h.AssertCaption(t, "en", "Hello 1")
			h.AssertNoCaption(t, "es")
			h.AssertFetchCount(t, "tt_r1_es.xml", 0) // not fetched eagerly
			// Both languages are still OFFERED to patrons.
			if got := len(h.State().AvailableLanguages); got != 2 {
				t.Errorf("available languages = %d, want 2", got)
			}

			if !h.Machine.RequestLanguage("es") {
				t.Fatalf("RequestLanguage(es) failed for an offered language")
			}
			h.Evaluate()
			h.AssertCaption(t, "es", "Hola 1")
			h.AssertCaption(t, "en", "Hello 1") // the language already rendering is untouched
			h.AssertFetchCount(t, "tt_r1_es.xml", 1)

			// The patron's language must survive the next reel boundary, not drop
			// back to the eager set.
			h.Send(UpdateTimeline(62000, "1", acsp.PlayPlaying))
			h.Evaluate()
			h.AssertCaption(t, "en", "Hello 2")
			h.AssertCaption(t, "es", "Hola 2")

			if h.Machine.RequestLanguage("de") {
				t.Errorf("RequestLanguage(de) must fail for a language the RPL does not carry")
			}
		},
	}
}

func mergeFiles(a, b map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
