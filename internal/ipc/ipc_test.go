package ipc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recvTimeout reads one Message from the server's channel or fails after a
// generous bound. Loopback unix socket delivery is effectively instant; the
// bound only guards against a hang.
func recvTimeout(t *testing.T, ch <-chan Message, d time.Duration) Message {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatalf("Messages() channel closed while awaiting a message")
		}
		return m
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for a Message", d)
		return Message{}
	}
}

// serveInBackground starts srv.Serve(ctx) on a goroutine and returns a channel
// that yields Serve's error once it returns.
func serveInBackground(t *testing.T, srv *Server, ctx context.Context) <-chan error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ctx) }()
	return errc
}

func TestServeDeliversSingleAndMultipleMessages(t *testing.T) {
	sock := tmpSock(t)
	srv, err := NewServer(sock)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := serveInBackground(t, srv, ctx)

	// A single connection carrying multiple NDJSON lines must surface every
	// framed message in order.
	msgs := []Message{
		{Type: "assistant", SessionID: "sess-1", Text: "first", Timestamp: 1},
		{Type: "assistant", SessionID: "sess-1", Text: "second", Timestamp: 2},
		{Type: "assistant", SessionID: "sess-1", Text: "third", Timestamp: 3},
	}

	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	for _, m := range msgs {
		line := mustMarshalLine(t, m)
		if _, err := conn.Write(line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_ = conn.Close()

	for i, want := range msgs {
		got := recvTimeout(t, srv.Messages(), 2*time.Second)
		if got != want {
			t.Fatalf("message %d: got %+v, want %+v", i, got, want)
		}
	}

	cancel()
	select {
	case <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestSendRoundTrip(t *testing.T) {
	sock := tmpSock(t)
	srv, err := NewServer(sock)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveInBackground(t, srv, ctx)

	msg := Message{Type: "assistant", SessionID: "abc", Text: "hello world", Timestamp: 42}
	if delivered := Send(sock, msg, 500*time.Millisecond); !delivered {
		t.Fatal("Send reported not delivered against a live server")
	}

	got := recvTimeout(t, srv.Messages(), 2*time.Second)
	if got != msg {
		t.Fatalf("delivered message mismatch: got %+v, want %+v", got, msg)
	}
}

func TestSendNonDeliveryPaths(t *testing.T) {
	t.Run("missing path", func(t *testing.T) {
		sock := filepath.Join(t.TempDir(), "nope.sock")
		if Send(sock, Message{Text: "x"}, 200*time.Millisecond) {
			t.Fatal("Send to a missing path returned true")
		}
	})

	t.Run("regular file (non-socket)", func(t *testing.T) {
		reg := filepath.Join(t.TempDir(), "regular.file")
		if err := os.WriteFile(reg, []byte("not a socket"), 0o600); err != nil {
			t.Fatalf("write regular file: %v", err)
		}
		if Send(reg, Message{Text: "x"}, 200*time.Millisecond) {
			t.Fatal("Send to a regular file returned true")
		}
	})

	t.Run("dead socket (no listener)", func(t *testing.T) {
		// Bind and immediately close a listener to leave a real socket file
		// with nothing accepting. lstat passes, dial fails => not delivered.
		sock := tmpSock(t)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		_ = ln.Close()
		// Closing a unix listener removes the socket file; recreate a real
		// socket node that has no listener by binding then closing the fd only.
		ln2, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("relisten: %v", err)
		}
		lf := ln2.(*net.UnixListener)
		lf.SetUnlinkOnClose(false)
		_ = lf.Close()
		defer os.Remove(sock)

		if Send(sock, Message{Text: "x"}, 200*time.Millisecond) {
			t.Fatal("Send to a socket with no listener returned true")
		}
	})
}

func TestNewServerRefusesNonSocketPath(t *testing.T) {
	reg := filepath.Join(t.TempDir(), "regular.file")
	if err := os.WriteFile(reg, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	srv, err := NewServer(reg)
	if err == nil {
		t.Fatal("NewServer accepted a pre-existing regular file; want error")
	}
	if srv != nil {
		t.Fatal("NewServer returned a non-nil Server alongside an error")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("unexpected error text: %v", err)
	}
	// The guard must not have unlinked the existing file.
	if _, statErr := os.Stat(reg); statErr != nil {
		t.Fatalf("guard removed the pre-existing regular file: %v", statErr)
	}
}

func TestNewServerRefusesLiveSocket(t *testing.T) {
	sock := tmpSock(t)
	srv1, err := NewServer(sock)
	if err != nil {
		t.Fatalf("first NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveInBackground(t, srv1, ctx)

	// Make sure the first server is actually accepting before probing.
	if delivered := Send(sock, Message{Text: "warmup"}, 500*time.Millisecond); !delivered {
		t.Fatal("first server never became live")
	}
	recvTimeout(t, srv1.Messages(), 2*time.Second)

	srv2, err := NewServer(sock)
	if err == nil {
		t.Fatal("second NewServer hijacked a live socket; want error")
	}
	if srv2 != nil {
		t.Fatal("second NewServer returned a non-nil Server alongside an error")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestServeCancelClosesMessagesAndRemovesSocket(t *testing.T) {
	sock := tmpSock(t)
	srv, err := NewServer(sock)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if _, statErr := os.Stat(sock); statErr != nil {
		t.Fatalf("socket file missing after NewServer: %v", statErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := serveInBackground(t, srv, ctx)

	cancel()

	select {
	case err := <-errc:
		if err != context.Canceled {
			t.Fatalf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}

	// Messages() must be closed once Serve returns.
	select {
	case _, ok := <-srv.Messages():
		if ok {
			t.Fatal("Messages() yielded a value after Serve returned; want closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Messages() not closed after Serve returned")
	}

	// The socket file is removed on shutdown.
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("socket file still present after shutdown: err=%v", statErr)
	}
}

func TestMalformedJSONLineSkippedButFollowingDelivered(t *testing.T) {
	sock := tmpSock(t)
	srv, err := NewServer(sock)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveInBackground(t, srv, ctx)

	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// An empty line, a malformed JSON line, then a valid message. Only the
	// valid one should surface, proving the bad lines are skipped in-stream
	// without dropping the connection.
	payload := "\n" + "{not json]\n" + string(mustMarshalLine(t, Message{
		Type: "assistant", SessionID: "s", Text: "survivor", Timestamp: 7,
	}))
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.Close()

	got := recvTimeout(t, srv.Messages(), 2*time.Second)
	if got.Text != "survivor" {
		t.Fatalf("expected the valid message after malformed lines, got %+v", got)
	}
}

func TestOversizedLineRejectedServerStaysAlive(t *testing.T) {
	sock := tmpSock(t)
	srv, err := NewServer(sock)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveInBackground(t, srv, ctx)

	// One connection sends a single line larger than the 1MB cap with no
	// newline. The server's scanner hits bufio.ErrTooLong and drops the
	// connection without ever surfacing a message.
	bad, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial oversized: %v", err)
	}
	oversized := make([]byte, maxLineBytes+1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	// Best-effort write; the server may close mid-write once the cap trips.
	_, _ = bad.Write(oversized)
	_ = bad.Close()

	// The server must remain healthy: a fresh connection still delivers.
	good := Message{Type: "assistant", SessionID: "s", Text: "after-oversize", Timestamp: 9}
	if delivered := Send(sock, good, 500*time.Millisecond); !delivered {
		t.Fatal("server did not accept a follow-up message after an oversized line")
	}
	got := recvTimeout(t, srv.Messages(), 2*time.Second)
	if got != good {
		t.Fatalf("follow-up mismatch: got %+v, want %+v", got, good)
	}
}

func mustMarshalLine(t *testing.T, m Message) []byte {
	t.Helper()
	// Mirror Send's wire framing: one JSON object followed by a newline.
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// tmpSock returns a unix-socket path short enough to satisfy the sockaddr_un
// sun_path limit (104 bytes on macOS). t.TempDir() lives under $TMPDIR
// (/var/folders/…), whose ~48-byte prefix plus the test name pushes longer-named
// tests past the cap and yields "bind: invalid argument"; binding under /tmp
// keeps the whole path comfortably short. The dir is removed on cleanup.
func tmpSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ccs-ipc")
	if err != nil {
		t.Fatalf("mkdir temp sock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}
