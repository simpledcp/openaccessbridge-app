// Package ipc defines the loopback IPC seam between the three supervised
// processes (buildplan.md §3.5). captionbridge-core supervises captionbridge-media
// and captionbridge-firewall and talks to them only over local loopback (Unix
// domain socket), never shared memory or a shared address space — so a media
// crash cannot drop the caption lease, and the privileged firewall helper stays a
// small, auditable, separate component.
//
// The wire format is newline-delimited JSON over a Unix domain socket: simple,
// debuggable, and dependency-free. Messages carry STATE and COMMANDS only; no
// caption text or audio samples ever cross this boundary in a form that is logged.
package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultSocketDir is where the loopback sockets live. On the appliance this is a
// tmpfs runtime dir; in dev it falls back to the OS temp dir.
func DefaultSocketDir() string {
	if d := os.Getenv("CAPTIONBRIDGE_RUNTIME_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "captionbridge")
	}
	return filepath.Join(os.TempDir(), "captionbridge")
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

	// Media process (captionbridge-media).
	TypeMediaStatus    = "media.status"     // helper → core: per-stream AudioStreamState
	TypeMediaConfigure = "media.configure"  // core → helper: device + mapping
	TypeMediaStart     = "media.start"      // core → helper: start a named stream
	TypeMediaStop      = "media.stop"       // core → helper: stop a named stream
	TypeMediaSignal    = "media.signal"     // WebRTC SDP/ICE signaling relay (opaque)

	// Firewall process (captionbridge-firewall).
	TypeFirewallApply  = "firewall.apply"   // core → helper: apply a profile's ruleset
	TypeFirewallVerify = "firewall.verify"  // core → helper: run verification pass
	TypeFirewallResult = "firewall.result"  // helper → core: VerificationReport
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

// Server listens on a Unix domain socket and dispatches frames to a Handler.
type Server struct {
	ln      net.Listener
	handler Handler
	wg      sync.WaitGroup
	mu      sync.Mutex
	closed  bool
}

// Listen binds a Unix domain socket at path (removing any stale socket first) and
// restricts it to the owner (0700 dir / 0600 socket) so only local, same-user
// processes can connect.
func Listen(path string, h Handler) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir socket dir: %w", err)
	}
	_ = os.Remove(path) // clear a stale socket from a prior crash
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	s := &Server{ln: ln, handler: h}
	s.wg.Add(1)
	go s.serve()
	return s, nil
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
