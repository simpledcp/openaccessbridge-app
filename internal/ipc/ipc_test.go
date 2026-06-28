package ipc

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestRoundTrip exercises the loopback request/response path used between the
// supervised processes (§3.5): a Ping is answered with a Pong over a Unix socket.
func TestRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "test.sock")
	srv, err := Listen(sock, func(m Message) *Message {
		if m.Type == TypePing {
			return &Message{Type: TypePong, ID: m.ID}
		}
		return &Message{Type: "echo", Payload: m.Payload, ID: m.ID}
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	cli, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	resp, err := cli.Request(Message{Type: TypePing, ID: "1"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Type != TypePong || resp.ID != "1" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	payload, _ := json.Marshal(map[string]string{"k": "v"})
	resp2, err := cli.Request(Message{Type: "x", Payload: payload, ID: "2"})
	if err != nil {
		t.Fatalf("Request2: %v", err)
	}
	if resp2.Type != "echo" || resp2.ID != "2" {
		t.Fatalf("unexpected echo: %+v", resp2)
	}
}
