// Package ipc defines the loopback IPC seam between the three supervised
// processes (buildplan.md §3.5). openaccessbridge-core supervises openaccessbridge-media
// and openaccessbridge-firewall and talks to them only over local loopback
// (Unix domain socket), never shared memory or a shared address space — so a
// media crash cannot drop the caption lease, and the privileged firewall helper
// stays a small, auditable, separate component.
//
// The wire format is newline-delimited JSON over a Unix domain socket: simple,
// debuggable, and dependency-free. Messages carry STATE and COMMANDS only; no
// caption text or audio samples ever cross this boundary in a form that is logged.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultSocketDir is where the loopback sockets live. On the appliance this is a
// tmpfs runtime dir; in dev it falls back to the OS temp dir.
func DefaultSocketDir() string {
	if d := os.Getenv("OPENACCESSBRIDGE_RUNTIME_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "openaccessbridge")
	}
	return filepath.Join(os.TempDir(), "openaccessbridge")
}

// Socket names for each helper process.
const (
	SocketMedia    = "media.sock"
	SocketFirewall = "firewall.sock"
)

// Message is a single IPC frame. Type identifies the command/event; Payload is an
// opaque JSON body the receiver interprets per Type.
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// ID correlates a request with its response (optional).
	ID string `json:"id,omitempty"`
}

// Common message types across the loopback boundary.
const (
	// Health/liveness (every helper answers Ping with Pong → drives §3.5
	// per-process liveness on the dashboard).
	TypePing = "ping"
	TypePong = "pong"

	// Media process (openaccessbridge-media).
	TypeMediaStatus    = "media.status"    // helper → core: per-stream AudioStreamState
	TypeMediaConfigure = "media.configure" // core → helper: device + mapping
	TypeMediaStart     = "media.start"     // core → helper: start a named stream
	TypeMediaStop      = "media.stop"      // core → helper: stop a named stream
	TypeMediaSignal    = "media.signal"    // WebRTC SDP/ICE signaling relay (opaque)

	// Firewall process (openaccessbridge-firewall).
	TypeFirewallApply  = "firewall.apply"  // core → helper: apply a profile's ruleset
	TypeFirewallVerify = "firewall.verify" // core → helper: run verification pass
	TypeFirewallResult = "firewall.result" // helper → core: VerificationReport
)

// Encode writes a message as a single newline-delimited JSON frame.
func Encode(w io.Writer, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// Handler processes an inbound message and may return a response (or nil).
type Handler func(Message) *Message

// Peer identifies the process on the other end of a loopback connection, as
// reported by the kernel at connect time (SO_PEERCRED on Linux).
type Peer struct {
	UID int
	GID int
	PID int
}

// AllowOwner authorizes only processes running as the same uid as the listener.
// This is the default policy and matches the historical owner-only socket.
func AllowOwner() func(Peer) bool {
	self := os.Getuid()
	return func(p Peer) bool { return p.UID == self }
}

// AllowOwnerOrGroup authorizes the listener's own uid, or any process whose
// PRIMARY gid is gid. This is what lets the unprivileged core talk to the root
// firewall helper: they run as different users but share the openaccessbridge
// group (§3.5). A gid of 0 authorizes nobody by group.
func AllowOwnerOrGroup(gid int) func(Peer) bool {
	self := os.Getuid()
	return func(p Peer) bool { return p.UID == self || (gid != 0 && p.GID == gid) }
}

// ListenOptions controls socket ownership and who may connect. The zero value is
// the owner-only posture Listen has always used.
type ListenOptions struct {
	// GroupGID, when non-zero, chowns the socket to that group so a peer running
	// as a different user in that group can connect. Required on the appliance,
	// where the firewall helper is root and core is not (§3.5).
	GroupGID int
	// Mode is the socket's permission bits (default 0600; use 0660 with GroupGID).
	Mode os.FileMode
	// DirMode is the socket directory's permission bits (default 0700; use 0750
	// with GroupGID so the group can traverse it).
	DirMode os.FileMode
	// AllowPeer authorizes a connecting process by its kernel-reported identity.
	// Nil means owner-only. Permissions alone are not the boundary: the
	// privileged helper must be able to attribute every command it accepts.
	AllowPeer func(Peer) bool
}

// Server listens on a Unix domain socket and dispatches frames to a Handler.
type Server struct {
	ln        net.Listener
	handler   Handler
	allowPeer func(Peer) bool
	wg        sync.WaitGroup
	mu        sync.Mutex
	closed    bool
	rejected  int
}

// Listen binds a Unix domain socket at path (removing any stale socket first) and
// restricts it to the owner (0700 dir / 0600 socket) so only local, same-user
// processes can connect. The directory mode applies when this call creates the
// directory; a directory that already exists is shared, and is left as the
// platform provisioned it (see prepareSocketDir).
func Listen(path string, h Handler) (*Server, error) {
	return ListenWithOptions(path, h, ListenOptions{})
}

// maxSocketPath is the usable length of a Unix socket path. The kernel's
// sockaddr_un.sun_path is 108 bytes including the terminating NUL, and it does
// not truncate: an over-long path fails bind(2) with the famously unhelpful
// EINVAL ("invalid argument"). Catch it here and say what is actually wrong.
const maxSocketPath = 107

func checkSocketPath(path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("socket path is %d bytes, over the %d-byte kernel limit: %s "+
			"(use a shorter --socket-dir)", len(path), maxSocketPath, path)
	}
	return nil
}

// ListenWithOptions is Listen with explicit socket ownership and an admission
// policy. Use it where the peer runs as a different user (§3.5).
func ListenWithOptions(path string, h Handler, opts ListenOptions) (*Server, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	dirMode := opts.DirMode
	if dirMode == 0 {
		dirMode = 0o700
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0o600
	}
	if err := prepareSocketDir(filepath.Dir(path), dirMode, opts.GroupGID); err != nil {
		return nil, err
	}

	_ = os.Remove(path) // clear a stale socket from a prior crash
	ln, err := listenUnixWithMode(path, mode)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	// Lchown, never Chown: the path must not be followed into whatever a
	// symlink dropped in its place points at (see listenUnixWithMode).
	if opts.GroupGID != 0 {
		if err := os.Lchown(path, -1, opts.GroupGID); err != nil {
			ln.Close()
			return nil, fmt.Errorf("chgrp socket: %w", err)
		}
	}
	// Verify, never repair: what is at the path must be the socket this
	// process just bound, with the identity it was given. Anything else means
	// the path was swapped underneath us, and a listener there is not ours.
	if err := verifySocketFile(path, mode, opts.GroupGID); err != nil {
		ln.Close()
		return nil, err
	}

	allow := opts.AllowPeer
	if allow == nil {
		allow = AllowOwner()
	}
	s := &Server{ln: ln, handler: h, allowPeer: allow}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// prepareSocketDir creates the socket directory with mode and group when it
// does not exist, and otherwise checks that the existing directory is one this
// process may share (checkSharedSocketDir). It never changes the mode or the
// ownership of a directory it did not create.
//
// On the appliance the runtime directory is provisioned by the platform —
// systemd's RuntimeDirectory=, a tmpfs volume — and shared by three processes
// under two accounts, the root helper and the unprivileged core/media user, each
// of which must be able to create its own socket in it. A process that
// "corrected" the shared directory to its own default would lock the others
// out: media tightening it to owner-only leaves the helper (root, but with a
// capability set of exactly CAP_NET_ADMIN and CAP_NET_RAW) unable to create its
// socket after a restart, and the helper widening it to its group-traverse mode
// leaves core unable to create the §9.4 debug-capture sink. Either way the
// failure is "operation not permitted" at the next restart, on the appliance
// only, and never on a dev host where the three run as one user.
func prepareSocketDir(dir string, mode os.FileMode, gid int) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, mode); err != nil {
			return fmt.Errorf("mkdir socket dir: %w", err)
		}
		// MkdirAll applies the umask; the mode is a decision, not a suggestion.
		if err := os.Chmod(dir, mode); err != nil {
			return fmt.Errorf("chmod socket dir: %w", err)
		}
		if gid != 0 {
			if err := os.Chown(dir, -1, gid); err != nil {
				return fmt.Errorf("chgrp socket dir: %w", err)
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat socket dir: %w", err)
	}
	owner, group, known := fileOwner(info)
	return checkSharedSocketDir(dir, dirIdentity{
		Mode: info.Mode(), UID: owner, GID: group, OwnerKnown: known,
	}, os.Getuid(), gid)
}

// verifySocketFile checks that path holds the socket this process just bound:
// a socket, not a link; owned by this process; in the requested group when one
// was requested; with exactly the requested permission bits. Where the platform
// reports no ownership, only the type is judged.
func verifySocketFile(path string, mode os.FileMode, gid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("verify socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("verify socket: %s is not a socket (mode %v); the path was replaced", path, info.Mode())
	}
	uid, fgid, known := fileOwner(info)
	if !known {
		return nil
	}
	if uid != os.Getuid() {
		return fmt.Errorf("verify socket: %s is owned by uid %d, not this process (uid %d); the path was replaced", path, uid, os.Getuid())
	}
	if gid != 0 && fgid != gid {
		return fmt.Errorf("verify socket: %s has gid %d, want %d", path, fgid, gid)
	}
	if got := info.Mode().Perm(); got != mode.Perm() {
		return fmt.Errorf("verify socket: %s has mode %04o, want %04o", path, got, mode.Perm())
	}
	return nil
}

// dirIdentity is what checkSharedSocketDir judges an existing directory by.
type dirIdentity struct {
	Mode     os.FileMode // full mode, including the type and sticky bits
	UID, GID int
	// OwnerKnown is false where the platform reports no owner (dirOwner);
	// ownership is then not judged.
	OwnerKnown bool
}

// checkSharedSocketDir decides whether an existing directory may hold this
// process's socket. It is a pure function of the directory's identity so the
// policy is testable without a second account or root.
//
// Accepted: a directory owned by this process, by root, or by the shared group
// the listener was configured with (the appliance's runtime directory is owned
// by the unprivileged account and group-shared with the root helper). Refused:
// a symlink — a socket path must not be redirectable; anything world-writable
// — any local user could replace the socket; and any other owner — a directory
// that is not ours, not the platform's and not the shared group's is a
// misconfiguration, and the answer to one is a named refusal, not a listener
// in somebody else's directory.
func checkSharedSocketDir(dir string, d dirIdentity, self, sharedGID int) error {
	if d.Mode&os.ModeSymlink != 0 {
		return fmt.Errorf("socket dir %s is a symlink; refusing to follow it", dir)
	}
	if !d.Mode.IsDir() {
		return fmt.Errorf("socket dir %s is not a directory", dir)
	}
	if d.Mode.Perm()&0o002 != 0 {
		return fmt.Errorf("socket dir %s is world-writable (mode %04o); any local user could replace the socket",
			dir, d.Mode.Perm())
	}
	if !d.OwnerKnown {
		return nil
	}
	switch {
	case d.UID == self, d.UID == 0:
		return nil
	case sharedGID != 0 && d.GID == sharedGID:
		return nil
	}
	return fmt.Errorf("socket dir %s is owned by uid %d, not by this process (uid %d), root, or the shared group; "+
		"it must be provisioned for the appliance's accounts (see deploy/README.md)", dir, d.UID, self)
}

// Rejected returns how many connections were refused by the admission policy.
// Surfaced as a count only — never the peer's identity (§9.4).
func (s *Server) Rejected() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	if !s.admit(conn) {
		return
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if resp := s.handler(m); resp != nil {
			if err := Encode(conn, *resp); err != nil {
				return
			}
		}
	}
}

// admit applies the peer-credential policy. It fails CLOSED: a peer whose
// identity the kernel will not report is refused, because the privileged helper
// must be able to attribute every command it acts on (§3.5).
func (s *Server) admit(conn net.Conn) bool {
	p, err := peerOf(conn)
	if err != nil || !s.allowPeer(p) {
		s.mu.Lock()
		s.rejected++
		s.mu.Unlock()
		return false
	}
	return true
}

// Close stops the server and waits for in-flight handlers.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

// Client is a connection to a helper's Unix domain socket.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

// Dial connects to a helper socket at path.
func Dial(path string) (*Client, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial unix %s: %w", path, err)
	}
	return &Client{conn: conn, r: bufio.NewReader(conn)}, nil
}

// Request sends a message and waits for a single response frame.
func (c *Client) Request(m Message) (Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := Encode(c.conn, m); err != nil {
		return Message{}, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return Message{}, err
	}
	var resp Message
	if err := json.Unmarshal(line, &resp); err != nil {
		return Message{}, err
	}
	return resp, nil
}

// Send fires a message without waiting for a response.
func (c *Client) Send(m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Encode(c.conn, m)
}

// Close closes the client connection.
func (c *Client) Close() error { return c.conn.Close() }
