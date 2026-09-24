package main

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"layeh.com/gumble/gumble"
)

func TestSenderRender(t *testing.T) {
	s := NewSender(1, 5)
	msg := "【麦序】<b>A&amp;B</b><br>1. x"
	if got := s.render(msg); got != msg {
		t.Errorf("html render = %q", got)
	}
	s.allowHTML.Store(false)
	if got := s.render(msg); got != "【麦序】A&B\n1. x" {
		t.Errorf("plain render = %q", got)
	}
	s.allowHTML.Store(true)
	s.maxLen.Store(20)
	long := "【麦序】排队：<br>1. aaaa<br>2. bbbb<br>3. cccc"
	got := s.render(long)
	if !strings.HasSuffix(got, "<br>……") || len([]rune(got)) > 20 {
		t.Errorf("truncated render = %q", got)
	}
}

func TestSenderCoalesce(t *testing.T) {
	s := NewSender(1, 5)
	s.ToChannel(1, "a", "status:1")
	s.ToChannel(1, "b", "")
	s.ToChannel(1, "c", "status:1")
	if m := s.pop(); m.html != "c" {
		t.Errorf("first = %q, want replaced status", m.html)
	}
	if m := s.pop(); m.html != "b" {
		t.Errorf("second = %q", m.html)
	}
	if s.pending() {
		t.Error("queue not empty")
	}
}

func TestSenderDropsPrivateMessagesFromOldConnection(t *testing.T) {
	s := NewSender(1, 5)
	s.ToUser(&gumble.Client{}, 7, "hi")
	// Session 7 may be someone else on a new connection; send must return
	// without touching the client (a nil client would panic).
	var newConn *gumble.Client
	s.send(newConn, s.pop())
}

func TestDoWithTimeoutRecoversPanic(t *testing.T) {
	if doWithTimeout(&gumble.Client{}, time.Second, func() { panic("boom") }) {
		t.Fatal("panicking call reported success")
	}
	if !doWithTimeout(&gumble.Client{}, time.Second, func() {}) {
		t.Fatal("normal call reported failure")
	}
}

// TestRunWithServerOffline runs the real connection loop against a port
// nobody listens on: it must keep retrying without crashing and stop
// promptly when cancelled.
func TestRunWithServerOffline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // now nothing listens there

	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Server = addr
	cfg.CertFile = filepath.Join(dir, "bot.crt")
	cfg.KeyFile = filepath.Join(dir, "bot.key")
	cfg.TrustFile = filepath.Join(dir, "server.fingerprint")
	cfg.StateFile = filepath.Join(dir, "state.json")
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	bot := NewBot(&cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { bot.Run(ctx); close(done) }()

	time.Sleep(4 * time.Second) // several failed attempts (1s, 2s backoff)
	select {
	case <-done:
		t.Fatal("Run returned while the server was offline")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

// TestRunWithSilentServer covers a server that accepts TCP connections but
// never answers (for example a frozen process): each attempt must time out
// instead of hanging forever.
func TestRunWithSilentServer(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			defer c.Close() // hold the connection open, say nothing
		}
	}()

	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Server = ln.Addr().String()
	cfg.CertFile = filepath.Join(dir, "bot.crt")
	cfg.KeyFile = filepath.Join(dir, "bot.key")
	cfg.TrustFile = filepath.Join(dir, "server.fingerprint")
	cfg.StateFile = filepath.Join(dir, "state.json")
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	bot := NewBot(&cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)

	// The first attempt hangs in the TLS handshake until dialTimeout, then a
	// retry follows after the 1s backoff.
	deadline := time.Now().Add(dialTimeout + 5*time.Second)
	for accepted.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no retry after a silent server; attempts = %d", accepted.Load())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestConfigDuration(t *testing.T) {
	var c struct {
		A, B, C, D Duration
	}
	if err := json.Unmarshal([]byte(`{"A": 45, "B": "2分钟", "C": "off", "D": "90s"}`), &c); err != nil {
		t.Fatal(err)
	}
	if time.Duration(c.A) != 45*time.Second || time.Duration(c.B) != 2*time.Minute || c.C != 0 || time.Duration(c.D) != 90*time.Second {
		t.Errorf("durations = %+v", c)
	}
	if err := json.Unmarshal([]byte(`{"A": "soon"}`), &c); err == nil {
		t.Error("want error for invalid duration")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address() != "mumble.example.com:64738" || time.Duration(cfg.WarnBefore) != 30*time.Second {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestConfigAddress(t *testing.T) {
	for in, want := range map[string]string{
		"mumble.example.com":      "mumble.example.com:64738",
		"mumble.example.com:1234": "mumble.example.com:1234",
		"::1":                     "[::1]:64738",
		"[::1]:5000":              "[::1]:5000",
	} {
		c := Config{Server: in}
		if got := c.Address(); got != want {
			t.Errorf("Address(%q) = %q, want %q", in, got, want)
		}
	}
}
