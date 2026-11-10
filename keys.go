// Key translation, pure Go (no Win32) so it can be unit-tested on Linux.
// Used by the terminal WM_CHAR / WM_KEYDOWN / mouse handlers in
// main_windows.go.
//
// Bug 2026-10-29 (reported vs PuTTY): while a Node.js script was
// waiting for input, pressing Backspace in KERM showed literal
// "^H^H^H..." instead of deleting characters, while PuTTY deleted
// them normally. Example from the user:
//   Server berjalan di http://localhost:1111
//   sdadsa^H^H^H^H^H
//
// Cause: Windows WM_CHAR for Backspace is 0x08 (^H), and KERM sent
// that byte straight to the server. A Linux pty's default erase
// character is 0x7F (DEL), not 0x08 — and xterm-256color terminfo
// (the TERM KERM requests) says kbs=\177. So the remote line
// discipline did not recognise 0x08 as erase: with ECHOCTL it echoed
// it back as the two printable characters '^' and 'H', and kept the
// 0x08 bytes in the input buffer. PuTTY, by default, sends DEL (127)
// for Backspace, which the remote recognises as erase and echoes as
// "\b \b" (cursor left, space, cursor left) — which KERM's VT already
// renders correctly ('\b' moves the cursor left, see vt.go).
//
// Fix: map Backspace (0x08, also produced by Ctrl+H) to DEL (0x7F)
// before sending, exactly like PuTTY's default. 0x7F itself stays
// 0x7F, and every other character passes through unchanged. This
// applies to SSH and Telnet (same WM_CHAR path). The password prompt
// is unaffected: it handles Backspace locally and never sends it.
//
// Audit 2026-10-29 (user asked "are there other codes still different
// from PuTTY?", fix-all requested 2026-10-29 23:03 WIB): the rest of
// this file brings the other key codes in line with PuTTY's xterm
// defaults. Modifier encoding is the xterm standard:
//   1 = none, 2 = Shift, 3 = Alt, 4 = Shift+Alt,
//   5 = Ctrl, 6 = Shift+Ctrl, 7 = Alt+Ctrl, 8 = Shift+Alt+Ctrl
// i.e. 1 + (Shift?1:0) + (Alt?2:0) + (Ctrl?4:0).
package main

import "fmt"

func keyCharToSend(r rune) []byte {
	if r == 0x08 {
		return []byte{0x7F}
	}
	return []byte(string(r))
}

// modifierCode returns the xterm modifier parameter (1 = none).
func modifierCode(shift, ctrl, alt bool) int {
	m := 1
	if shift {
		m += 1
	}
	if alt {
		m += 2
	}
	if ctrl {
		m += 4
	}
	return m
}

// arrowSeq builds an arrow-key sequence. dir is 'A' (up), 'B' (down),
// 'C' (right) or 'D' (left). appCursor is DECCKM (CSI ? 1 h): in
// application mode PuTTY sends SS3 (ESC O …), otherwise CSI
// (ESC [ …). Unmodified sequences omit the ";1" parameter, exactly
// like xterm/PuTTY.
func arrowSeq(dir byte, appCursor, shift, ctrl, alt bool) []byte {
	mod := modifierCode(shift, ctrl, alt)
	if mod == 1 {
		if appCursor {
			return []byte{0x1B, 'O', dir}
		}
		return []byte{0x1B, '[', dir}
	}
	if appCursor {
		return []byte(fmt.Sprintf("\x1bO1;%d%c", mod, dir))
	}
	return []byte(fmt.Sprintf("\x1b[1;%d%c", mod, dir))
}

// homeEndSeq builds a Home ('H') / End ('F') sequence, same
// normal/application rules as arrowSeq.
func homeEndSeq(final byte, appCursor, shift, ctrl, alt bool) []byte {
	mod := modifierCode(shift, ctrl, alt)
	if mod == 1 {
		if appCursor {
			return []byte{0x1B, 'O', final}
		}
		return []byte{0x1B, '[', final}
	}
	if appCursor {
		return []byte(fmt.Sprintf("\x1bO1;%d%c", mod, final))
	}
	return []byte(fmt.Sprintf("\x1b[1;%d%c", mod, final))
}

// tildeSeq builds a "~" sequence (Insert 2, Delete 3, PgUp 5, PgDn 6,
// F5 15, F6 17, F7 18, F8 19, F9 20, F10 21, F11 23, F12 24), with an
// optional ";modifier" parameter when any modifier is held.
func tildeSeq(code int, shift, ctrl, alt bool) []byte {
	mod := modifierCode(shift, ctrl, alt)
	if mod == 1 {
		return []byte(fmt.Sprintf("\x1b[%d~", code))
	}
	return []byte(fmt.Sprintf("\x1b[%d;%d~", code, mod))
}

// fKeySeq builds an F1-F4 sequence: SS3 (ESC O P/Q/R/S) unmodified,
// CSI 1;modifier P/Q/R/S when modified — exactly like xterm/PuTTY.
func fKeySeq(final byte, shift, ctrl, alt bool) []byte {
	mod := modifierCode(shift, ctrl, alt)
	if mod == 1 {
		return []byte{0x1B, 'O', final}
	}
	return []byte(fmt.Sprintf("\x1b[1;%d%c", mod, final))
}

// metaSeq builds an Alt+letter (Meta) sequence: ESC followed by the
// character itself, like PuTTY. Only used for printable ASCII and
// DEL (Alt+Backspace = ESC + DEL, since Backspace itself is DEL);
// other inputs return nil.
func metaSeq(r rune) []byte {
	if (r >= 0x20 && r <= 0x7E) || r == 0x7F {
		return []byte{0x1B, byte(r)}
	}
	return nil
}

// tabSeq builds a Tab sequence: Shift+Tab is Backtab (ESC[Z) in
// PuTTY/xterm, plain Tab is 0x09.
func tabSeq(shift bool) []byte {
	if shift {
		return []byte("\x1b[Z")
	}
	return []byte{0x09}
}

// mouseSeq builds one mouse-reporting sequence. button is the base
// button plus modifier/motion bits already combined by the caller:
// 0 = left, 1 = middle, 2 = right, 64 = wheel up, 65 = wheel down,
// +4 Shift, +8 Alt, +16 Ctrl, +32 motion (drag / no-button move).
// x, y are 1-based cell coordinates. sgr selects the SGR (1006)
// encoding (ESC[<btn;x;yM for press/motion/wheel, ESC[<btn;x;ym for
// release, where btn is the button released, not 3); otherwise the
// X10 encoding is used (ESC[M + 3 bytes, release = button 3).
// release=true means a button release event.
func mouseSeq(button, x, y int, sgr, release bool) []byte {
	if x < 1 {
		x = 1
	}
	if y < 1 {
		y = 1
	}
	if sgr {
		final := byte('M')
		if release {
			final = 'm'
		}
		return []byte(fmt.Sprintf("\x1b[<%d;%d;%d%c", button, x, y, final))
	}
	b := button
	if release {
		// X10 release: button 3, modifiers preserved (bits 4/8/16),
		// motion bit cleared.
		b = 3 | (button & (4 | 8 | 16))
	}
	// X10 coordinates are single bytes offset by 32; clamp to the
	// encodable range (PuTTY/xterm do the same for old-style reports).
	cx := x + 32
	if cx > 255 {
		cx = 255
	}
	cy := y + 32
	if cy > 255 {
		cy = 255
	}
	return []byte{0x1B, '[', 'M', byte(b + 32), byte(cx), byte(cy)}
}

// bracketedPasteWrap wraps pasted text in the bracketed-paste markers
// (ESC[200~ ... ESC[201~) when the application enabled mode 2004,
// otherwise returns the text unchanged. The caller already translated
// newlines to '\r' before calling this.
func bracketedPasteWrap(text string, enabled bool) string {
	if !enabled {
		return text
	}
	return "\x1b[200~" + text + "\x1b[201~"
}

// mouseButton combines a base mouse button with modifier bits, in
// the encoding shared by SGR and X10 reports (before the release
// conversion in mouseSeq): Shift +4, Alt +8, Ctrl +16, motion +32.
func mouseButton(base int, shift, ctrl, alt, motion bool) int {
	b := base
	if shift {
		b += 4
	}
	if alt {
		b += 8
	}
	if ctrl {
		b += 16
	}
	if motion {
		b += 32
	}
	return b
}
