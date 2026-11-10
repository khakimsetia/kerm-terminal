package main

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"crypto/rsa"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Local SSH server + Connect() end-to-end test (Linux only, no Windows UI).
func TestConnectPassword(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{}
	cfg.PasswordCallback = func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
		if c.User() == "tester" && string(pass) == "secret" {
			return nil, nil
		}
		return nil, errAuth
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for ch := range chans {
			if ch.ChannelType() != "session" {
				ch.Reject(ssh.UnknownChannelType, "unknown")
				continue
			}
			channel, requests, err := ch.Accept()
			if err != nil {
				continue
			}
			go func() {
				for req := range requests {
					if req.Type == "pty-req" || req.Type == "shell" {
						req.Reply(true, nil)
					} else if req.Type == "window-change" {
						req.Reply(true, nil)
					} else {
						req.Reply(false, nil)
					}
				}
			}()
			go func() {
				buf := make([]byte, 1024)
				for {
					n, err := channel.Read(buf)
					if n > 0 {
						_, _ = channel.Write([]byte("\x1b[32mgreen\x1b[0m echo:"))
						_, _ = channel.Write(buf[:n])
					}
					if err != nil {
						break
					}
				}
			}()
			_ = sc
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	term := NewTerm(80, 24)
	outCh := make(chan []byte, 16)
	c, err := Connect(Session{Host: "127.0.0.1", Port: addr.Port, Username: "tester", Auth: "password", Password: "secret"},
		80, 24, func(host, fp, warn string) bool { return true },
		func(b []byte) { outCh <- b }, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	if _, err := c.Stdin.Write([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case b := <-outCh:
			term.Feed(b)
			if term.LineText(term.LinesForView(0), 0) != "" {
				// check colour + text present
				lines := term.LinesForView(0)
				if lines[0][0].Fg == ansi256(2) {
					return
				}
			}
		case <-timeout:
			t.Fatalf("timeout waiting for coloured echo, screen=%q", term.LineText(term.LinesForView(0), 0))
		}
	}
}

var errAuth = &authError{}

type authError struct{}

func (e *authError) Error() string { return "auth failed" }

func TestSessionSecretsNeverMarshaled(t *testing.T) {
	// BUG 2026-10-29: password & passphrase sempat tersimpan
	// plaintext di sessions.json. Test ini memastikan marshal
	// Session TIDAK PERNAH memuat nilai rahasia itu.
	s := Session{Name: "x", Host: "h", Password: "SECRET-PW", KeyPass: "SECRET-KP", KeyData: []byte("SECRET-KEY")}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-PW", "SECRET-KP", "SECRET-KEY"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("session JSON must not contain secret %q, got %s", secret, b)
		}
	}
	if !strings.Contains(string(b), `"Host":"h"`) {
		t.Fatalf("session JSON must still contain host, got %s", b)
	}
}
