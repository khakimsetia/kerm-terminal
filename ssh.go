// SSH connection handling — pure Go, no Windows dependency.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Session struct {
	Name     string // saved-session name (persistence only)
	Host     string
	Port     int
	Protocol string // "ssh" (default, also when empty) or "telnet"
	GreenPrompt bool // Recolor prompt client-side (user@host: green, dir blue), no server command
	Username string
	Auth     string // "password" or "key"
	// Password, KeyPass, KeyData TIDAK PERNAH disimpan ke file.
	// BUG 2026-10-29 (ditemukan saat audit vs PuTTY): sebelumnya
	// field ini tidak punya tag json, jadi json.Marshal di
	// saveSessions() menulis password & passphrase key dalam
	// plaintext ke sessions.json, padahal status UI berkata
	// "(password not saved)" dan prinsip KERM adalah password
	// tidak pernah dipersist. PuTTY juga menolak menyimpan
	// password sesi. Tag json:"-" ini + pembersihan eksplisit
	// di save/loadSessions() memastikan itu tidak terjadi lagi.
	// Password tetap diminta di terminal tiap Connect (prompt),
	// passphrase key tetap diminta di panel tiap Connect.
	Password string `json:"-"`
	KeyPath  string // saved key file path (persistence only)
	KeyComment string // display label for key auth: .ppk Comment, else key file basename
	KeyData  []byte `json:"-"`
	KeyPass  string `json:"-"`
}

type Conn struct {
	Client  *ssh.Client
	Session *ssh.Session
	Stdin   interface{ Write([]byte) (int, error) }

	// Telnet fields (nil for SSH connections).
	Telnet       net.Conn
	telnetWrite  func([]byte) error
	telnetMu     sync.Mutex
	telnetNAWS   bool
	telnetParser *telnetParser
}

func configDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "SSHTerminal")
}

func knownHostsPath() string { return filepath.Join(configDir(), "known_hosts.txt") }
func sessionsPath() string   { return filepath.Join(configDir(), "sessions.json") }

func loadKnownHosts() map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(knownHostsPath())
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m
}

func saveKnownHost(host, fp string) {
	dir := configDir()
	_ = os.MkdirAll(dir, 0700)
	f, err := os.OpenFile(knownHostsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(host + " " + fp + "\n")
}

// ConfirmFunc is called on the UI thread (via the caller) when a host key
// is unknown or changed. Returning false aborts the connection.
type ConfirmFunc func(host, fingerprint, warning string) bool

// Connect dials SSH and opens a shell session with a PTY.
// onOutput is called (from a background goroutine) for every chunk of
// server output; it must be thread-safe (the UI synchronises it).
// onClosed is called once when the session ends, with the error from
// Session.Wait() (nil = clean exit/logout, non-nil = abnormal
// disconnect — network drop, sleep, server restart, reset, etc.).
// The caller maps it to a user-facing explanation (see disconnect.go).
func Connect(cfg Session, cols, rows int, confirm ConfirmFunc, onOutput func([]byte), onClosed func(error)) (*Conn, error) {
	if cfg.Host == "" || cfg.Username == "" {
		return nil, errors.New("host and username are required")
	}
	port := cfg.Port
	if port <= 0 || port > 65535 {
		port = 22
	}
	var auth []ssh.AuthMethod
	if cfg.Auth == "key" {
		keyData := cfg.KeyData
		if len(keyData) == 0 {
			return nil, errors.New("select a key file first")
		}
		if strings.Contains(string(keyData), "PuTTY-User-Key-File-") {
			pem, _, err := PPKToOpenSSH(string(keyData), cfg.KeyPass)
			if err != nil {
				return nil, fmt.Errorf("failed to read .ppk: %w", err)
			}
			keyData = pem
		}
		var signer ssh.Signer
		var err error
		if cfg.KeyPass != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(cfg.KeyPass))
		} else {
			signer, err = ssh.ParsePrivateKey(keyData)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read key: %w", err)
		}
		auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	} else {
		// Password BOLEH kosong di sini: UI tidak lagi gagal duluan
		// ("password belum diisi"). Kalau field password kosong, UI
		// meminta password di dalam terminal setelah popup terminal
		// muncul, lalu Connect dipanggil dengan password itu. Kalau
		// password tetap kosong (user langsung Enter), tetap dicoba —
		// server yang akan menerima/menolak, bukan UI yang memblokir
		// sebelum connect. KeyboardInteractive ditambahkan dengan
		// password yang sama, supaya server yang hanya mengizinkan
		// challenge-response juga bisa login tanpa prompt kedua.
		pw := cfg.Password
		auth = []ssh.AuthMethod{
			ssh.Password(pw),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				ans := make([]string, len(questions))
				for i := range ans {
					ans[i] = pw
				}
				return ans, nil
			}),
		}
	}

	known := loadKnownHosts()
	hostID := fmt.Sprintf("%s:%d", cfg.Host, port)
	sshCfg := &ssh.ClientConfig{
		User:            cfg.Username,
		Auth:            auth,
		Timeout:         10 * time.Second,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			sum := sha256.Sum256(key.Marshal())
			fp := "SHA256:" + base64.StdEncoding.EncodeToString(sum[:])
			// strip trailing '=' like OpenSSH does
			fp = strings.TrimRight(fp, "=")
			if old, ok := known[hostID]; ok {
				if old == fp {
					return nil
				}
				if confirm != nil && confirm(hostID, fp, "WARNING: host key fingerprint CHANGED from the previously saved one! This may be a different server / someone impersonating it.") {
					return nil
				}
				return errors.New("host key changed — connection cancelled")
			}
			if confirm != nil {
				if confirm(hostID, fp, "") {
					saveKnownHost(hostID, fp)
					return nil
				}
				return errors.New("host key not confirmed — connection cancelled")
			}
			return nil
		},
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH failed: %w", err)
	}
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to open session: %w", err)
	}
	// Prompt hijau kini TIDAK lagi lewat perintah PS1 ke server (itu
	// yang membuat prompt jadi 2x). Prompt diwarnai di sisi klien oleh
	// emulator VT (Term.PromptRecolor, lihat vt.go), tanpa perintah
	// apa pun ke server, jadi tidak ada prompt tambahan sama sekali.
	// PTY kembali ke ECHO=1 normal.
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("failed to open PTY: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				onOutput(cp)
			}
			if err != nil {
				break
			}
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				onOutput(cp)
			}
			if err != nil {
				break
			}
		}
	}()
	if err := sess.Shell(); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("failed to open shell: %w", err)
	}
	// Keepalive (seperti PuTTY, yang sebelumnya tidak ada di KERM):
	// kirim keepalive@openssh.com tiap 60 detik supaya koneksi idle
	// tidak diputus NAT/router di tengah jalan. Goroutine berhenti
	// sendiri begitu request gagal (koneksi sudah mati) — ia TIDAK
	// menutup sesi sendiri, supaya penanganan disconnect (banner
	// 3 baris, sleep, dll) tetap lewat sess.Wait() seperti biasa.
	// Telnet tidak dikirim keepalive: NOP Telnet bisa mengotori
	// perangkat LAN yang bukan server Telnet murni.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				return
			}
		}
	}()
	go func() {
		waitErr := sess.Wait()
		if onClosed != nil {
			onClosed(waitErr)
		}
	}()
	c := &Conn{Client: client, Session: sess, Stdin: stdin}
	_ = c
	return c, nil
}

// SendWindowChange tells the server the terminal was resized.
func (c *Conn) SendWindowChange(cols, rows int) {
	if c == nil {
		return
	}
	if c.Telnet != nil {
		// Telnet NAWS, only if the server agreed to it.
		c.telnetMu.Lock()
		ok := c.telnetNAWS
		c.telnetMu.Unlock()
		if ok && c.telnetWrite != nil {
			_ = c.telnetWrite([]byte{255, 250, 31, byte(cols >> 8), byte(cols), byte(rows >> 8), byte(rows), 255, 240})
		}
		return
	}
	if c.Session != nil {
		_ = c.Session.WindowChange(rows, cols)
	}
}

func (c *Conn) Close() {
	if c == nil {
		return
	}
	if c.Telnet != nil {
		_ = c.Telnet.Close()
		return
	}
	if c.Session != nil {
		_ = c.Session.Close()
	}
	if c.Client != nil {
		_ = c.Client.Close()
	}
}
