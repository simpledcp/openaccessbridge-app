package web

// The §11.5 pre-show QA checklist, on the operator surface.
//
// Two representations, because a checklist that only exists in a browser is not
// a procedure: JSON for the dashboard tile, and PLAIN TEXT for the booth. The
// text form is the one that matters — it prints, it goes in a binder, it can be
// read on a phone in a projection booth with the lights down, and it survives
// the appliance being the thing that is broken.
//
// It is admin-only. The sheet names the cinema server, the interface assignment
// and the room's faults; none of that belongs on the patron network (§8.4).

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/readiness"
)

// handleAdminPreShow serves the §11.5 sheet. `?format=text` returns the
// printable form.
func (s *Server) handleAdminPreShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cl := s.preShowChecklist()
	if strings.EqualFold(r.URL.Query().Get("format"), "text") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(renderChecklistText(cl)))
		return
	}
	writeJSON(w, cl)
}

// preShowChecklist assembles the sheet from the SAME status the dashboard
// renders, plus the session facts this package owns.
//
// With no Status dependency wired there is no measurement to report, and the
// sheet says exactly that rather than being built from a zero value — an
// all-zero SystemReadiness would render as a page of failures and would send a
// booth chasing faults that were never measured.
func (s *Server) preShowChecklist() model.PreShowChecklist {
	facts := readiness.ChecklistFacts{
		// The RESOLVED name, not the configured default: an appliance nobody named
		// must fail this item rather than pass it on a flag's build-time value.
		Auditorium:           s.auditoriumName(),
		PerShowTokenRequired: s.cfg.PerShowToken,
	}
	// §8.5's re-cabling defence: the label the installer recorded, printed for a
	// person to check against the rack.
	if s.deps.Store != nil {
		if profiles, err := s.deps.Store.ListAuditoriumProfiles(); err == nil {
			for _, p := range profiles {
				if strings.TrimSpace(p.PhysicalPortLabel) != "" {
					facts.PhysicalPortLabel = p.PhysicalPortLabel
					break
				}
			}
		}
	}
	if cur, ok := s.sessions.Current(); ok {
		facts.SessionIssued = true
		facts.SessionExpiresAt = cur.ExpiresAt
		facts.JoinCodeIssued = cur.JoinCode != ""
	}
	if s.deps.Status == nil {
		return model.PreShowChecklist{
			Auditorium:  s.auditoriumName(),
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Checks: []model.PreShowCheck{{
				ID: "status_unavailable", Title: "The appliance's own readiness could not be read",
				Section: "§11.5", Source: model.SourceMeasured, State: model.CheckNotRun, Blocking: true,
				Detail: "this surface has no link to the readiness supervisor, so nothing on the sheet " +
					"below could be measured",
				Remedy: "Do not open the house on this sheet. Check captionbridge-core.",
			}},
		}
	}
	return readiness.BuildChecklist(s.deps.Status(), facts)
}

// checklistWidth is the column the printable sheet wraps at. Narrow enough for a
// booth printer and for a phone held sideways in a dark projection room, which
// is where this sheet is actually read.
const checklistWidth = 78

// wrap lays a paragraph out under an item, hanging-indented. A remedy is the
// part of this sheet a person acts on, and an unwrapped 200-column instruction
// is one that scrolls off a printed page.
func wrap(text, prefix string, contPrefix ...string) string {
	cont := prefix
	if len(contPrefix) > 0 {
		cont = contPrefix[0]
	}
	width := checklistWidth - len(cont)
	if width < 20 {
		width = 20
	}
	var b strings.Builder
	line := prefix
	lineLen := 0
	for _, word := range strings.Fields(text) {
		if lineLen > 0 && lineLen+1+len(word) > width {
			b.WriteString(line + "\n")
			line, lineLen = cont, 0
		}
		if lineLen > 0 {
			line += " "
			lineLen++
		}
		line += word
		lineLen += len(word)
	}
	if lineLen > 0 {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderChecklistText is the booth's copy: fixed width, no colour, no
// interaction, and the remedy printed UNDER each item that is not passing.
//
// The remedy is printed only where it is needed, because a sheet that repeats an
// instruction under every line — including the twelve that are fine — is a sheet
// nobody reads to the bottom. That is not a cosmetic judgement: this page is
// consulted by someone who is already late.
func renderChecklistText(cl model.PreShowChecklist) string {
	var b strings.Builder
	title := "CaptionBridge pre-show checklist"
	if cl.Auditorium != "" {
		title += " — " + cl.Auditorium
	}
	fmt.Fprintf(&b, "%s\n", title)
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", len(title)))
	fmt.Fprintf(&b, "Generated %s. These are measurements taken at that moment, not a standing state.\n\n", cl.GeneratedAt)

	for _, c := range cl.Checks {
		marker := "[ ]"
		switch c.State {
		case model.CheckPass:
			marker = "[x]"
		case model.CheckFail:
			marker = "[!]"
		case model.CheckUnmeasurable:
			marker = "[-]"
		}
		scope := ""
		if c.Blocking {
			scope = "  (BLOCKING)"
		}
		who := ""
		if c.Source == model.SourceStaff {
			who = "  (STAFF STEP — the appliance cannot check this)"
		}
		fmt.Fprintf(&b, "%s %-54s %s%s%s\n", marker, c.Title, c.State.Label(), scope, who)
		b.WriteString(wrap(c.Detail+"  ["+c.Section+"]", "      "))
		if c.State != model.CheckPass {
			b.WriteString(wrap(c.Remedy, "      -> ", "         "))
		}
		b.WriteString("\n")
	}

	b.WriteString(strings.Repeat("-", 72) + "\n")
	if cl.ApplianceReady {
		b.WriteString("APPLIANCE: every blocking check it can make is passing.\n")
	} else {
		fmt.Fprintf(&b, "APPLIANCE: NOT READY — %s\n", strings.Join(cl.BlockingProblems, ", "))
	}
	// The line that keeps the sheet honest. The appliance's own verdict must
	// never be read as "the room is ready": it cannot see the seats, the SSID or
	// the loaner drawer, and the half it knows must not vouch for the half it
	// does not.
	fmt.Fprintf(&b, "STAFF: %d step(s) above are yours and are NOT recorded here. The appliance\n"+
		"       does not know whether they were done, and this sheet never claims it does.\n",
		cl.StaffStepsOutstanding)
	return b.String()
}
