package sim

import (
	"sort"
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
		Desc: "A status keepalive near expiry renews the lease; captions continue.",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 10000, 0, rpl, true)
			h.AdvanceTick(8000 * time.Millisecond) // near expiry (warning territory)
			h.AssertState(t, model.StatePlaying)
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
		Desc: "Set Output Mode disabled/enabled drives the patron 'no captions' vs 'active' state (§5.2 #3).",
		Opts: HarnessOptions{Files: files},
		Run: func(t T, h *Harness) {
			connect(h, 30000, 0, rpl, true)
			h.AdvanceEval(1000 * time.Millisecond)
			h.AssertCaption(t, "en", "Captioned")
			h.Send(SetOutputMode(false))
			if got := h.PatronStatus("en"); got != string(model.PatronCaptionNoCaptions) {
				t.Errorf("disabled output status = %q, want no_captions", got)
			}
			h.Send(SetOutputMode(true))
			if got := h.PatronStatus("en"); got != string(model.PatronCaptionActive) {
				t.Errorf("re-enabled output status = %q, want active", got)
			}
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
