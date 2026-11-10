package main

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

func TestTelnetParserNormal(t *testing.T) {
	p := newTelnetParser(80, 24)
	clean, resp := p.Feed([]byte("login: "))
	if string(clean) != "login: " || len(resp) != 0 {
		t.Fatalf("normal: clean=%q resp=%v", clean, resp)
	}
}

func TestTelnetParserIACEscape(t *testing.T) {
	p := newTelnetParser(80, 24)
	clean, _ := p.Feed([]byte("a\xff\xffb"))
	if string(clean) != "a\xffb" {
		t.Fatalf("IAC escape: %q", clean)
	}
}

func TestTelnetParserNegotiation(t *testing.T) {
	p := newTelnetParser(80, 24)
	// Server: WILL ECHO -> DO ECHO; DO NAWS -> WILL NAWS + NAWS size.
	_, resp := p.Feed([]byte{255, 251, 1, 255, 253, 31})
	want := []byte{255, 253, 1, 255, 251, 31, 255, 250, 31, 0, 80, 0, 24, 255, 240}
	if !bytes.Equal(resp, want) {
		t.Fatalf("negotiation resp=%v want=%v", resp, want)
	}
	if !p.nawsOK {
		t.Fatal("nawsOK should be true after DO NAWS")
	}
}

func TestTelnetParserTermType(t *testing.T) {
	p := newTelnetParser(80, 24)
	// Server: SB TERMINAL-TYPE SEND SE -> SB TERMINAL-TYPE IS xterm-256color SE.
	_, resp := p.Feed([]byte{255, 250, 24, 1, 255, 240})
	want := []byte{255, 250, 24, 0}
	want = append(want, []byte("xterm-256color")...)
	want = append(want, 255, 240)
	if !bytes.Equal(resp, want) {
		t.Fatalf("termtype resp=%v want=%v", resp, want)
	}
}

func TestTelnetParserSplitIAC(t *testing.T) {
	p := newTelnetParser(80, 24)
	c1, _ := p.Feed([]byte("hi\xff"))
	c2, _ := p.Feed([]byte{253, 31})
	if string(c1) != "hi" || string(c2) != "" {
		t.Fatalf("split IAC: c1=%q c2=%q", c1, c2)
	}
	if !p.nawsOK {
		t.Fatal("split DO NAWS should set nawsOK")
	}
}

func TestTelnetWriterNewlines(t *testing.T) {
	var out []byte
	w := &telnetWriter{write: func(b []byte) error { out = append(out, b...); return nil }}
	if _, err := w.Write([]byte("a\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("b\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("c\xffd")); err != nil {
		t.Fatal(err)
	}
	want := []byte("a\r\nb\r\nc\xff\xffd")
	if !bytes.Equal(out, want) {
		t.Fatalf("writer out=%q want=%q", out, want)
	}
}

func TestConnectTelnetEndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	var mu sync.Mutex
	var got []byte
	closed := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Send a login prompt (plain, no Telnet commands).
		_, _ = c.Write([]byte("login: "))
		// Read client input until CRLF.
		buf := make([]byte, 64)
		var in []byte
		for {
			n, err := c.Read(buf)
			if n > 0 {
				in = append(in, buf[:n]...)
				if bytes.Contains(in, []byte("\r\n")) {
					break
				}
			}
			if err != nil {
				t.Errorf("server read: %v", err)
				return
			}
		}
		if string(in) != "root\r\n" {
			t.Errorf("server got %q want %q", in, "root\r\n")
		}
		_, _ = c.Write([]byte("welcome\r\n"))
		// Close -> client onClosed should fire.
	}()
	cfg := Session{Host: "127.0.0.1", Port: port, Protocol: "telnet"}
	c, err := ConnectTelnet(cfg, 80, 24, func(b []byte) {
		mu.Lock()
		got = append(got, b...)
		mu.Unlock()
	}, func(error) { close(closed) })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Stdin.Write([]byte("root\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("onClosed not called after server closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Contains(got, []byte("login: ")) || !bytes.Contains(got, []byte("welcome")) {
		t.Fatalf("output %q missing login/welcome", got)
	}
}
