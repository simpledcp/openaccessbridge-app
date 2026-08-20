package debugcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// harness builds a Manager over a temp sink with a controllable clock. tmpfs is
// not required here so the fixtures run anywhere; the tmpfs requirement itself is
// covered by TestRequireTmpfs and TestIsTmpfs_RealMountTable.
func harness(t *testing.T) (*Manager, *time.Time, *bytes.Buffer, string) {
	t.Helper()
	now := time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "capture")
	var buf bytes.Buffer
	m := New(Options{
		Dir: dir,
		Log: logging.New(logging.Options{Component: "core", Writer: &buf, JSON: true}),
		Now: func() time.Time { return now },
	})
	return m, &now, &buf, dir
}

// safeClock is a controllable clock shared with the sweeper goroutine.
type safeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *safeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *safeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func grant() Grant {
	return Grant{Kind: KindCaption, TTL: time.Minute, MaxBytes: 64, GrantedBy: "admin", Reason: "captions look wrong"}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func events(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, ln)
		}
		out = append(out, m)
	}
	return out
}

// TestExplicitAndAdminOnly: §9.4 requires a capture to be "explicit, admin-only".
// Nothing captures until an identified admin grants it.
func TestExplicitAndAdminOnly(t *testing.T) {
	m, _, _, dir := harness(t)

	if m.Active() {
		t.Fatal("a fresh manager must not be capturing")
	}
	if _, err := m.Write(KindCaption, []byte("cue")); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("write without a grant = %v, want ErrNoGrant", err)
	}
	if len(files(t, dir)) != 0 {
		t.Fatalf("a refused write created files: %v", files(t, dir))
	}

	g := grant()
	g.GrantedBy = "  "
	if _, err := m.Start(g); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("an unidentified grant = %v, want ErrNotAdmin", err)
	}
	if _, err := m.Start(Grant{Kind: "screenshots", GrantedBy: "admin"}); err == nil {
		t.Fatal("an unknown capture kind should be refused")
	}
	if m.Active() {
		t.Fatal("no capture should be active after refusals")
	}
}

// TestTimeLimited: the grant's TTL is clamped to the ceiling, and the capture
// auto-deletes when the window closes — with no sweeper running, because Write
// and Sessions sweep inline too.
func TestTimeLimited(t *testing.T) {
	m, now, _, dir := harness(t)

	g := grant()
	g.TTL = 24 * time.Hour // over the ceiling
	st, err := m.Start(g)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if want := now.Add(DefaultMaxTTL); !st.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want the clamped %v", st.ExpiresAt, want)
	}
	if len(files(t, dir)) != 1 {
		t.Fatalf("want one capture file, got %v", files(t, dir))
	}

	*now = now.Add(DefaultMaxTTL + time.Second)
	if _, err := m.Write(KindCaption, []byte("late")); !errors.Is(err, ErrExpired) {
		t.Fatalf("write after expiry = %v, want ErrExpired", err)
	}
	if m.Active() {
		t.Error("an expired capture must not report active")
	}
	if got := files(t, dir); len(got) != 0 {
		t.Errorf("an expired capture must auto-delete its file, found %v", got)
	}
}

// TestSizeLimited: the byte budget is clamped and enforced. Writes past it are
// refused loudly (an event) rather than silently truncated.
func TestSizeLimited(t *testing.T) {
	m, _, _, _ := harness(t)

	g := grant()
	g.MaxBytes = 1 << 40 // over the ceiling
	st, err := m.Start(g)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.ByteBudget != DefaultAudioMaxByte {
		t.Errorf("ByteBudget = %d, want the clamped %d", st.ByteBudget, DefaultAudioMaxByte)
	}

	m2, _, buf2, dir2 := harness(t)
	if _, err := m2.Start(grant()); err != nil { // 64-byte budget
		t.Fatalf("Start: %v", err)
	}
	if n, err := m2.Write(KindCaption, bytes.Repeat([]byte("x"), 60)); err != nil || n != 60 {
		t.Fatalf("Write within budget = %d, %v", n, err)
	}
	if _, err := m2.Write(KindCaption, bytes.Repeat([]byte("x"), 10)); !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("write past the budget = %v, want ErrBudgetSpent", err)
	}
	// The refused bytes never reached the sink.
	data, err := os.ReadFile(filepath.Join(dir2, files(t, dir2)[0]))
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if len(data) != 60 {
		t.Errorf("sink holds %d bytes, want 60 (nothing past the budget)", len(data))
	}
	var capped bool
	for _, e := range events(t, buf2) {
		if e["event"] == string(logging.EvDebugCaptureCapped) {
			capped = true
		}
	}
	if !capped {
		t.Error("exhausting the budget must be loud, not silent")
	}
}

// TestVisiblyIndicated: §9.4 requires the dashboard to show a capture is running
// for the whole window (§8.3 warning set), and to stop showing it after.
func TestVisiblyIndicated(t *testing.T) {
	m, now, _, _ := harness(t)

	if len(m.Warnings()) != 0 {
		t.Fatal("no capture, no indicator")
	}
	if _, err := m.Start(grant()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ws := m.Warnings()
	if len(ws) != 1 {
		t.Fatalf("want one indicator, got %v", ws)
	}
	if ws[0].Code != model.WarnDebugCaptureActive {
		t.Errorf("code = %q, want %q", ws[0].Code, model.WarnDebugCaptureActive)
	}
	if ws[0].Severity != model.SeverityWarning {
		t.Errorf("severity = %q, want %q (a capture is authorized, but never invisible)", ws[0].Severity, model.SeverityWarning)
	}
	if !strings.Contains(ws[0].Message, "ACTIVE") {
		t.Errorf("the indicator must be loud: %q", ws[0].Message)
	}

	*now = now.Add(2 * time.Minute)
	if len(m.Warnings()) != 0 {
		t.Error("the indicator must clear once the window closes")
	}
}

// TestStopDeletesImmediately covers the admin ending a capture early.
func TestStopDeletesImmediately(t *testing.T) {
	m, _, buf, dir := harness(t)
	if _, err := m.Start(grant()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Write(KindCaption, []byte("some captured text")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := m.Stop(KindCaption); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Errorf("Stop must delete the capture, found %v", got)
	}
	if err := m.Stop(KindCaption); !errors.Is(err, ErrNoGrant) {
		t.Errorf("second Stop = %v, want ErrNoGrant", err)
	}
	var stopped bool
	for _, e := range events(t, buf) {
		if e["event"] == string(logging.EvDebugCaptureStopped) && e["deleted"] == true {
			stopped = true
		}
	}
	if !stopped {
		t.Error("the end of the window must be audited")
	}
}

// TestKindsAreSeparateGrants: enabling caption logging must not enable audio
// capture, and a second grant of the same kind is refused.
func TestKindsAreSeparateGrants(t *testing.T) {
	m, _, _, _ := harness(t)
	if _, err := m.Start(grant()); err != nil {
		t.Fatalf("Start caption: %v", err)
	}
	if _, err := m.Write(KindAudio, []byte("samples")); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("audio write under a caption grant = %v, want ErrNoGrant", err)
	}
	if _, err := m.Start(grant()); !errors.Is(err, ErrActive) {
		t.Fatalf("second caption grant = %v, want ErrActive", err)
	}
	audio := grant()
	audio.Kind = KindAudio
	if _, err := m.Start(audio); err != nil {
		t.Fatalf("Start audio: %v", err)
	}
	if len(m.Sessions()) != 2 || len(m.Warnings()) != 2 {
		t.Errorf("both captures should be live and indicated: %v", m.Sessions())
	}
}

// TestAuditTrailCarriesNoIdentityOrContent: the audit events name a HASHED admin
// and the bounds, never the admin's username and never the captured bytes.
func TestAuditTrailCarriesNoIdentityOrContent(t *testing.T) {
	m, _, buf, _ := harness(t)
	g := grant()
	g.GrantedBy = "booth-admin-jane"
	if _, err := m.Start(g); err != nil {
		t.Fatalf("Start: %v", err)
	}
	secret := "A SECRET CAPTION LINE"
	if _, err := m.Write(KindCaption, []byte(secret)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := m.Stop(KindCaption); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "booth-admin-jane") {
		t.Errorf("the admin identity leaked into the audit trail: %s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("captured content leaked into the log: %s", out)
	}
	if !strings.Contains(out, logging.HashID("admin", "booth-admin-jane")) {
		t.Errorf("the audit trail must carry the hashed grantee: %s", out)
	}
}

// TestRunSweepsAndClosesOnCancel: the timer path deletes an expired capture, and
// cancelling the context deletes whatever is still open.
func TestRunSweepsAndClosesOnCancel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capture")
	clk := &safeClock{now: time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)}
	m := New(Options{Dir: dir, Log: logging.Nop(), Now: clk.Now})
	if _, err := m.Start(grant()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, time.Millisecond); close(done) }()

	// Expire the window and wait for the sweeper to notice.
	clk.advance(2 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for len(files(t, dir)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the sweeper did not delete the expired capture: %v", files(t, dir))
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	// A capture still open at shutdown is deleted too.
	clk.advance(-2 * time.Minute)
	if _, err := m.Start(grant()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Close()
	if got := files(t, dir); len(got) != 0 {
		t.Errorf("Close must delete open captures, found %v", got)
	}
}

// TestRequireTmpfs: §9.4 allows the sink to be a bounded tmpfs, because that is
// what makes "auto-delete on reboot" a property of the medium. A non-tmpfs sink
// is refused.
func TestRequireTmpfs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capture")
	mounts := filepath.Join(t.TempDir(), "mounts")

	// A fixture mount table in which the temp dir is on ext4.
	if err := os.WriteFile(mounts, []byte("/dev/sda1 / ext4 rw 0 0\n"), 0o600); err != nil {
		t.Fatalf("write mounts: %v", err)
	}
	m := New(Options{Dir: dir, RequireTmpfs: true, ProcMounts: mounts, Log: logging.Nop()})
	if _, err := m.Start(grant()); !errors.Is(err, ErrNotTmpfs) {
		t.Fatalf("a persistent-filesystem sink = %v, want ErrNotTmpfs", err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Errorf("a refused start must not create files: %v", got)
	}

	// Now claim the same path is tmpfs; the capture is allowed.
	line := "tmpfs " + filepath.Dir(dir) + " tmpfs rw 0 0\n"
	if err := os.WriteFile(mounts, []byte("/dev/sda1 / ext4 rw 0 0\n"+line), 0o600); err != nil {
		t.Fatalf("write mounts: %v", err)
	}
	if _, err := m.Start(grant()); err != nil {
		t.Fatalf("a tmpfs sink should be accepted: %v", err)
	}
	m.Close()
}

// TestIsTmpfs_RealMountTable checks the mount-table introspection against this
// host's real /proc/mounts: /dev/shm is tmpfs on any Linux, and the repository
// checkout is not.
func TestIsTmpfs_RealMountTable(t *testing.T) {
	if _, err := os.Stat(DefaultProcMounts); err != nil {
		t.Skip("no /proc/mounts on this host")
	}
	if _, err := os.Stat("/dev/shm"); err == nil {
		ok, err := IsTmpfs("/dev/shm/captionbridge-does-not-exist-yet", DefaultProcMounts)
		if err != nil {
			t.Fatalf("IsTmpfs(/dev/shm): %v", err)
		}
		if !ok {
			t.Error("/dev/shm should be reported as memory-backed")
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	ok, err := IsTmpfs(wd, DefaultProcMounts)
	if err != nil {
		t.Fatalf("IsTmpfs(%s): %v", wd, err)
	}
	if ok {
		t.Skipf("the source tree at %s is itself memory-backed; nothing to assert", wd)
	}
}

// TestIsTmpfs_LongestMountWins pins the matching rule: a nested mount takes
// precedence over its parent, and escaped mount points are decoded.
func TestIsTmpfs_LongestMountWins(t *testing.T) {
	root := t.TempDir()
	mounts := filepath.Join(root, "mounts")
	nested := filepath.Join(root, "run", "cb space")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	table := "/dev/sda1 / ext4 rw 0 0\n" +
		"/dev/sda2 " + filepath.Join(root, "run") + " ext4 rw 0 0\n" +
		"tmpfs " + strings.ReplaceAll(nested, " ", `\040`) + " tmpfs rw 0 0\n"
	if err := os.WriteFile(mounts, []byte(table), 0o600); err != nil {
		t.Fatalf("write mounts: %v", err)
	}
	ok, err := IsTmpfs(filepath.Join(nested, "sub"), mounts)
	if err != nil {
		t.Fatalf("IsTmpfs: %v", err)
	}
	if !ok {
		t.Error("the nested tmpfs mount should win over its ext4 parent")
	}
	ok, err = IsTmpfs(filepath.Join(root, "run"), mounts)
	if err != nil {
		t.Fatalf("IsTmpfs: %v", err)
	}
	if ok {
		t.Error("the parent mount is ext4 and must not be reported as tmpfs")
	}
}

// TestCaptureFilePermissions: the sink is admin-only on disk too.
func TestCaptureFilePermissions(t *testing.T) {
	m, _, _, dir := harness(t)
	if _, err := m.Start(grant()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("capture directory mode = %o, want 700", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, files(t, dir)[0]))
	if err != nil {
		t.Fatalf("stat capture: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("capture file mode = %o, want 600", perm)
	}
}

// TestGrantReasonNeverReachesTheLog: the grant's Reason is unbounded operator
// free text, typed while looking at a caption fault, so it can quote the caption
// itself. The redaction chokepoint (logging/redact.go) keys off field NAMES, and
// "reason" names nothing denied — so the only safe handling is not to log the
// text at all. The audit trail keeps whether a reason was given and how long it
// was; the text stays in the in-memory Status the dashboard renders (§9.4,
// guardrail #6).
func TestGrantReasonNeverReachesTheLog(t *testing.T) {
	m, _, buf, _ := harness(t)
	g := grant()
	g.Reason = "patron says the cue read WE ATTACK AT DAWN and was one line late"

	st, err := m.Start(g)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The dashboard still gets it: this is suppression in the LOG, not loss.
	if st.Reason != g.Reason {
		t.Errorf("Status.Reason = %q, want the operator's note back", st.Reason)
	}
	if err := m.Stop(KindCaption); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	out := buf.String()
	for _, fragment := range []string{"WE ATTACK AT DAWN", "one line late", g.Reason} {
		if strings.Contains(out, fragment) {
			t.Errorf("operator free text reached the log stream (%q):\n%s", fragment, out)
		}
	}
	// The audit trail must still record that a bounded, reasoned grant happened.
	evs := events(t, buf)
	if len(evs) == 0 {
		t.Fatal("no audit events emitted")
	}
	start := evs[0]
	if start["reason_given"] != true {
		t.Errorf("start event should record that a reason was given: %v", start)
	}
	if start["reason_len"] != float64(len(g.Reason)) {
		t.Errorf("reason_len = %v, want %d", start["reason_len"], len(g.Reason))
	}
	if _, ok := start["reason"]; ok {
		t.Errorf("the start event must carry no free-text reason field: %v", start)
	}
}
