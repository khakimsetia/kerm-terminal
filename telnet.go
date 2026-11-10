// Telnet client — pure Go, no Windows dependency.
// Telnet is UNENCRYPTED: everything (including passwords typed at the
// server's login prompt) travels in cleartext. This is only for local /
// trusted devices that do not speak SSH (routers, ONT, lab gear). The UI
// warns about this whenever Telnet is selected.
//
// Protocol handling: strip Telnet commands (IAC sequences) from the server
// stream, answer the common negotiations, and escape IAC + translate
// newlines on the way out. Login itself is interactive: the user types
// username/password at the server's prompt inside the terminal, exactly
// like a classic telnet client — there is no username/password field
// in the protocol itself.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	telIAC  = 255 // Interpret As Command
	telDONT = 254
	telDO   = 253
	telWONT = 252
	telWILL = 251
	telSB   = 250 // Subnegotiation Begin
	telSE   = 240 // Subnegotiation End

	telOptEcho    = 1
	telOptSGA     = 3 // Suppress Go Ahead
	telOptTerm    = 24
	telOptNAWS    = 31 // Negotiate About Window Size
	telTermSend   = 1
	telTermIs     = 0
)

// telnetParser is a state machine that consumes raw Telnet bytes and
// returns (clean output, response bytes to send back). It keeps its state
// across Feed calls, so an IAC split across two TCP chunks is handled.
type telnetParser struct {
	state    int
	opt      byte
	sbOpt    byte
	sbData   []byte
	nawsOK   bool
	cols     int
	rows     int
	resp     []byte
	clean    []byte
}

const (
	pNormal = iota
	pIAC
	pWill
	pWont
	pDo
	pDont
	pSB
	pSBData
	pSBIAC
)

func newTelnetParser(cols, rows int) *telnetParser {
	return &telnetParser{cols: cols, rows: rows}
}

func (p *telnetParser) nawsBytes() []byte {
	return []byte{telIAC, telSB, telOptNAWS,
		byte(p.cols >> 8), byte(p.cols),
		byte(p.rows >> 8), byte(p.rows),
		telIAC, telSE}
}

func (p *telnetParser) Feed(data []byte) (clean, resp []byte) {
	p.clean = p.clean[:0]
	p.resp = p.resp[:0]
	for _, b := range data {
		switch p.state {
		case pNormal:
			if b == telIAC {
				p.state = pIAC
			} else {
				p.clean = append(p.clean, b)
			}
		case pIAC:
			switch b {
			case telIAC:
				p.clean = append(p.clean, telIAC)
				p.state = pNormal
			case telWILL:
				p.state = pWill
			case telWONT:
				p.state = pWont
			case telDO:
				p.state = pDo
			case telDONT:
				p.state = pDont
			case telSB:
				p.state = pSB
			default:
				// NOP, DM, BRK, IP, AO, AYT, EC, EL, GA, etc. — ignore.
				p.state = pNormal
			}
		case pWill:
			// Server WILL <opt>: agree only to ECHO and SGA.
			if b == telOptEcho || b == telOptSGA {
				p.resp = append(p.resp, telIAC, telDO, b)
			} else {
				p.resp = append(p.resp, telIAC, telDONT, b)
			}
			p.state = pNormal
		case pWont:
			p.state = pNormal
		case pDo:
			// Server DO <opt>: agree to NAWS, terminal-type, SGA.
			switch b {
			case telOptNAWS:
				p.resp = append(p.resp, telIAC, telWILL, b)
				p.resp = append(p.resp, p.nawsBytes()...)
				p.nawsOK = true
			case telOptTerm:
				p.resp = append(p.resp, telIAC, telWILL, b)
			case telOptSGA:
				p.resp = append(p.resp, telIAC, telWILL, b)
			default:
				p.resp = append(p.resp, telIAC, telWONT, b)
			}
			p.state = pNormal
		case pDont:
			p.state = pNormal
		case pSB:
			p.sbOpt = b
			p.sbData = p.sbData[:0]
			p.state = pSBData
		case pSBData:
			if b == telIAC {
				p.state = pSBIAC
			} else {
				p.sbData = append(p.sbData, b)
			}
		case pSBIAC:
			if b == telIAC {
				p.sbData = append(p.sbData, telIAC)
				p.state = pSBData
			} else if b == telSE {
				// End of subnegotiation.
				if p.sbOpt == telOptTerm && len(p.sbData) > 0 && p.sbData[0] == telTermSend {
					p.resp = append(p.resp, telIAC, telSB, telOptTerm, telTermIs)
					p.resp = append(p.resp, []byte("xterm-256color")...)
					p.resp = append(p.resp, telIAC, telSE)
				}
				p.state = pNormal
			} else {
				// Unexpected byte after IAC inside SB — treat as end.
				p.state = pNormal
			}
		}
	}
	// Copy out, so the next Feed cannot overwrite the caller's slices.
	clean = append([]byte(nil), p.clean...)
	resp = append([]byte(nil), p.resp...)
	return clean, resp
}

// telnetWriter escapes IAC and translates newlines to Telnet's CRLF.
// A "\r\n" pair sent as two writes (or in one write) becomes exactly one
// CRLF, not two.
type telnetWriter struct {
	mu     sync.Mutex
	write  func([]byte) error
	prevCR bool
}

func (w *telnetWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []byte
	for _, b := range p {
		switch {
		case b == telIAC:
			out = append(out, telIAC, telIAC)
			w.prevCR = false
		case b == '\r':
			out = append(out, '\r', '\n')
			w.prevCR = true
		case b == '\n':
			if !w.prevCR {
				out = append(out, '\r', '\n')
			}
			w.prevCR = false
		default:
			out = append(out, b)
			w.prevCR = false
		}
	}
	if len(out) > 0 {
		if err := w.write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// ConnectTelnet dials a Telnet server and returns a Conn that behaves
// like the SSH Conn (Stdin.Write, SendWindowChange, Close, onOutput,
// onClosed). The reader loop parses Telnet commands and forwards only
// clean terminal output. onClosed receives the read error (nil =
// clean close: for Telnet, the server closing TCP *is* a clean
// session end, e.g. logout, so io.EOF is normalised to nil).
func ConnectTelnet(cfg Session, cols, rows int, onOutput func([]byte), onClosed func(error)) (*Conn, error) {
	if cfg.Host == "" {
		return nil, errors.New("host is required")
	}
	port := cfg.Port
	if port <= 0 || port > 65535 {
		port = 23
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	nc, err := d.Dial("tcp", net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", port)))
	if err != nil {
		return nil, err
	}
	c := &Conn{Telnet: nc}
	c.telnetWrite = func(b []byte) error {
		c.telnetMu.Lock()
		defer c.telnetMu.Unlock()
		_, err := nc.Write(b)
		return err
	}
	w := &telnetWriter{write: c.telnetWrite}
	c.Stdin = w
	parser := newTelnetParser(cols, rows)
	c.telnetParser = parser
	var once sync.Once
	closed := func(readErr error) {
		once.Do(func() {
			if onClosed != nil {
				onClosed(readErr)
			}
		})
	}
	go func() {
		var readErr error
		defer func() { closed(readErr) }()
		r := bufio.NewReader(nc)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				clean, resp := parser.Feed(buf[:n])
				if len(resp) > 0 {
					_ = c.telnetWrite(resp)
				}
				if len(clean) > 0 {
					onOutput(clean)
				}
				if parser.nawsOK {
					c.telnetMu.Lock()
					c.telnetNAWS = true
					c.telnetMu.Unlock()
				}
			}
			if err != nil {
				// Telnet: server menutup TCP = akhir sesi yang bersih
				// (mis. logout), bukan putus tidak normal.
				if err == io.EOF {
					readErr = nil
				} else {
					readErr = err
				}
				return
			}
		}
	}()
	return c, nil
}
