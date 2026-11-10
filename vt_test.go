package main

import (
	"strings"
	"testing"
)

func text(t *Term) []string {
	lines := t.LinesForView(0)
	out := make([]string, len(lines))
	for i := range lines {
		out[i] = t.LineText(lines, i)
	}
	return out
}

func TestPlainAndNewline(t *testing.T) {
	term := NewTerm(10, 4)
	term.Feed([]byte("hello\r\nworld"))
	got := text(term)
	if got[0] != "hello" || got[1] != "world" {
		t.Fatalf("got %q", got)
	}
}

func TestCursorAndSGR(t *testing.T) {
	term := NewTerm(10, 4)
	term.Feed([]byte("\x1b[31mred\x1b[0m ok"))
	lines := term.LinesForView(0)
	if lines[0][0].Ch != 'r' || lines[0][0].Fg != ansi256(1) {
		t.Fatalf("red cell = %+v", lines[0][0])
	}
	if lines[0][4].Ch != 'o' || !lines[0][4].FgDefault {
		t.Fatalf("after reset cell = %+v", lines[0][4])
	}
	term.Feed([]byte("\x1b[2;3H"))
	if term.CursorY != 1 || term.CursorX != 2 {
		t.Fatalf("cursor = %d,%d", term.CursorX, term.CursorY)
	}
}

func TestScrollAndAlt(t *testing.T) {
	term := NewTerm(5, 2)
	term.Feed([]byte("a\r\nb\r\nc"))
	if term.ScrollbackLen() != 1 {
		t.Fatalf("scrollback = %d", term.ScrollbackLen())
	}
	got := text(term)
	if got[0] != "b" || got[1] != "c" {
		t.Fatalf("got %q", got)
	}
	term.Feed([]byte("\x1b[?1049hXYZ"))
	if !term.AltActive() {
		t.Fatalf("alt not active")
	}
	term.Feed([]byte("\x1b[?1049l"))
	if term.AltActive() {
		t.Fatalf("alt still active")
	}
	got = text(term)
	if got[0] != "b" {
		t.Fatalf("after alt got %q", got)
	}
}

func TestWrap(t *testing.T) {
	term := NewTerm(4, 3)
	term.Feed([]byte("abcdef"))
	got := text(term)
	if !strings.Contains(got[0], "abcd") || !strings.Contains(got[1], "ef") {
		t.Fatalf("wrap got %q", got)
	}
}

func TestColorsPromptAndLs(t *testing.T) {
	// Simulasi prompt Termux/Linux + output ls --color:
	// user@host hijau tebal (01;32), direktori biru tebal (01;34),
	// executable hijau tebal (01;32), file biasa hijau (0;32).
	term := NewTerm(40, 4)
	term.Feed([]byte("\x1b[01;32mroot@server\x1b[00m$ \x1b[01;34mdir1\x1b[00m \x1b[01;32mrun.sh\x1b[00m\r\n\x1b[0;32mfile.txt\x1b[00m"))
	lines := term.LinesForView(0)
	// "root@server" di kolom 0-10: hijau + bold.
	if lines[0][0].Ch != 'r' || lines[0][0].Fg != ansi256(2) || !lines[0][0].Bold {
		t.Fatalf("prompt cell = %+v, want green+bold", lines[0][0])
	}
	// "$ " di 11-12, "dir1" mulai 13: biru + bold.
	if lines[0][13].Ch != 'd' || lines[0][13].Fg != ansi256(4) || !lines[0][13].Bold {
		t.Fatalf("dir cell = %+v, want blue+bold", lines[0][13])
	}
	// "run.sh" mulai 18: hijau + bold.
	if lines[0][18].Ch != 'r' || lines[0][18].Fg != ansi256(2) || !lines[0][18].Bold {
		t.Fatalf("exe cell = %+v, want green+bold", lines[0][18])
	}
	// Baris 2: "file.txt" hijau tanpa bold.
	if lines[1][0].Ch != 'f' || lines[1][0].Fg != ansi256(2) || lines[1][0].Bold {
		t.Fatalf("file cell = %+v, want green non-bold", lines[1][0])
	}
}

func TestLinesForViewOffset(t *testing.T) {
	term := NewTerm(10, 3)
	term.Feed([]byte("line1\r\nline2\r\nline3\r\nline4"))
	if term.ScrollbackLen() != 1 {
		t.Fatalf("scrollback = %d, want 1", term.ScrollbackLen())
	}
	// Live view: line2,line3,line4 (line1 scrolled off)
	live := term.LinesForView(0)
	if got := term.LineText(live, 0); got != "line2" {
		t.Fatalf("live row0 = %q, want line2", got)
	}
	// Scrolled up 1: line1,line2,line3
	up := term.LinesForView(1)
	if got := term.LineText(up, 0); got != "line1" {
		t.Fatalf("scrolled row0 = %q, want line1", got)
	}
	if got := term.LineText(up, 2); got != "line3" {
		t.Fatalf("scrolled row2 = %q, want line3", got)
	}
}

func TestSGRBareResetNanoExit(t *testing.T) {
	// Bug nano 2026-10-29: reset SGR tanpa parameter (\x1b[m, bagian
	// dari sgr0 terminfo "\E(B\E[m" yang dikirim nano/curses saat
	// keluar) diabaikan, sehingga reverse/background bocor ke shell.
	term := NewTerm(40, 4)
	term.Feed([]byte("\x1b[7mSTATUS\x1b[mNORMAL"))
	lines := term.LinesForView(0)
	// "STATUS" reverse, "NORMAL" sesudah reset bare harus normal.
	if !lines[0][0].Reverse {
		t.Fatalf("STATUS cell should be reverse, got %+v", lines[0][0])
	}
	// NORMAL mulai kolom 6.
	if lines[0][6].Reverse || !lines[0][6].FgDefault || !lines[0][6].BgDefault {
		t.Fatalf("NORMAL cell after bare reset = %+v, want default, no reverse", lines[0][6])
	}
	// Simulasi keluar nano: alt on, reverse aktif, sgr0, alt off,
	// lalu prompt shell. Prompt tidak boleh reverse/background.
	term2 := NewTerm(40, 4)
	term2.PromptRecolor = true
	term2.Feed([]byte("shell-before\r\n\x1b[?1049hnano-text\x1b[7mBAR\x1b(B\x1b[m\x1b[?1049l"))
	term2.Feed([]byte("root@myazure:~# "))
	lines2 := term2.LinesForView(0)
	// Cari baris prompt (baris terakhir yang berisi root@).
	found := false
	for _, row := range lines2 {
		if len(row) > 0 && row[0].Ch == 'r' {
			found = true
			if row[0].Reverse || !row[0].BgDefault {
				t.Fatalf("prompt cell after nano exit = %+v, want no reverse, default bg", row[0])
			}
		}
	}
	if !found {
		t.Fatalf("prompt row not found, lines=%q", text(term2))
	}
}

func TestPromptRecolorClearsReverse(t *testing.T) {
	// Pertahanan kedua: kalaupun reverse sempat bocor ke baris prompt
	// (mis. reset terlewat), recolor prompt harus membersihkannya,
	// supaya prompt hijau/biru tampil sebagai teks di atas hitam,
	// bukan teks hitam di atas background hijau/biru.
	term := NewTerm(40, 4)
	term.PromptRecolor = true
	term.Feed([]byte("\x1b[7mroot@myazure:~# "))
	lines := term.LinesForView(0)
	if lines[0][0].Reverse || !lines[0][0].BgDefault {
		t.Fatalf("recolored prompt cell = %+v, want no reverse, default bg", lines[0][0])
	}
}

func TestProgressBarWrapCR(t *testing.T) {
	// Bug progress bar 2026-10-29: bar yang melebihi lebar terminal
	// terpotong menjadi 2 baris fisik, lalu \r + bar berikutnya dulu
	// menambah satu baris baru per update (puluhan baris "Progress:
	// [ 99%]" di screenshot apt upgrade). Sekarang \r kembali ke
	// awal baris logis, jadi bar berikutnya menimpa baris logis yang
	// sama dan tidak pernah menambah baris.
	term := NewTerm(20, 10)
	bar := "Progress: [ 99%] [##########]" // 29 karakter > 20 kolom
	for i := 0; i < 5; i++ {
		term.Feed([]byte(bar + "\r"))
	}
	got := text(term)
	if got[0] != "Progress: [ 99%] [##" || got[1] != "########]" {
		t.Fatalf("wrapped progress got %q, want 2 stable lines", got[:3])
	}
	for i := 2; i < 10; i++ {
		if got[i] != "" {
			t.Fatalf("progress leaked to row %d: %q (full %q)", i, got[i], got)
		}
	}
	if term.CursorY != 0 || term.CursorX != 0 {
		t.Fatalf("cursor after final CR = %d,%d, want 0,0", term.CursorX, term.CursorY)
	}
}

func TestProgressBarWrapCRLF(t *testing.T) {
	// \r\n sesudah baris terpotong harus mendarat di baris sesudah
	// baris logis itu, bukan di baris sesudah baris pertamanya
	// (yang akan menimpa fragmen baris logis itu).
	term := NewTerm(20, 10)
	term.Feed([]byte("Progress: [ 99%] [##########]\r\nNEXT"))
	got := text(term)
	if got[0] != "Progress: [ 99%] [##" || got[1] != "########]" || got[2] != "NEXT" {
		t.Fatalf("CRLF after wrapped line got %q, want bar on 0-1, NEXT on 2", got[:4])
	}
}

func TestProgressBarFitsCR(t *testing.T) {
	// Bar yang muat tetap update di tempat yang sama (1 baris).
	term := NewTerm(80, 10)
	term.Feed([]byte("Progress: [ 99%] [####]\rProgress: [ 99%] [#####]\r"))
	got := text(term)
	if got[0] != "Progress: [ 99%] [#####]" || got[1] != "" {
		t.Fatalf("fitting progress got %q", got[:2])
	}
}

func TestProgressBarWrapAfterResize(t *testing.T) {
	// Sesudah diperbesar, update progress berikutnya tidak boleh
	// terus menambah baris baru per update.
	term := NewTerm(20, 10)
	bar := "Progress: [ 99%] [##########]"
	term.Feed([]byte(bar + "\r"))
	term.Resize(40, 15)
	// Bar baru muat di lebar baru (40): update di tempat, stabil.
	for i := 0; i < 5; i++ {
		term.Feed([]byte("Progress: [ 99%] [################]\r"))
	}
	got := text(term)
	nonBlank := 0
	for _, l := range got {
		if l != "" {
			nonBlank++
		}
	}
	// Paling banyak: 1 baris lama yang tertinggal + 1 baris live
	// (fragmen lama) — bukan 5 baris baru.
	if nonBlank > 2 {
		t.Fatalf("after resize progress lines = %d, want <=2, got %q", nonBlank, got)
	}
}

func TestCursorColorsGreenOnly(t *testing.T) {
	// Kursor harus selalu hijau, tidak ikut warna sel di bawahnya:
	// blink on = hijau muda (bright PuTTY), blink off = hijau tua
	// (normal PuTTY), teks selalu hitam supaya terbaca di atas blok.
	fg, bg := cursorColors(true)
	if fg != 0x000000 || bg != 0x55FF55 {
		t.Fatalf("cursor on = fg %06X bg %06X, want 000000 / 55FF55", fg, bg)
	}
	fg, bg = cursorColors(false)
	if fg != 0x000000 || bg != 0x00BB00 {
		t.Fatalf("cursor off = fg %06X bg %06X, want 000000 / 00BB00", fg, bg)
	}
}

func TestModesDECCKMMouseBracketed(t *testing.T) {
	term := NewTerm(80, 24)
	term.Feed([]byte("\x1b[?1h"))
	if !term.AppCursor {
		t.Fatalf("DECCKM ?1h must set AppCursor")
	}
	term.Feed([]byte("\x1b[?1000h\x1b[?1006h\x1b[?2004h"))
	if term.MouseMode != 1 || !term.MouseSGR || !term.BracketedPaste {
		t.Fatalf("mouse/bracketed modes not tracked: mode=%d sgr=%v bracket=%v", term.MouseMode, term.MouseSGR, term.BracketedPaste)
	}
	term.Feed([]byte("\x1b[?1002h"))
	if term.MouseMode != 2 {
		t.Fatalf("1002 must upgrade MouseMode to 2, got %d", term.MouseMode)
	}
	term.Feed([]byte("\x1b[?1l\x1b[?1002l\x1b[?1006l\x1b[?2004l"))
	if term.AppCursor || term.MouseMode != 0 || term.MouseSGR || term.BracketedPaste {
		t.Fatalf("mode off must clear: app=%v mouse=%d sgr=%v bracket=%v", term.AppCursor, term.MouseMode, term.MouseSGR, term.BracketedPaste)
	}
}

func TestDSR(t *testing.T) {
	term := NewTerm(80, 24)
	term.Feed([]byte("\x1b[5;3H"))
	resp := term.Feed([]byte("\x1b[6n"))
	if string(resp) != "\x1b[5;3R" {
		t.Fatalf("DSR 6n: got %q, want CPR 5;3", resp)
	}
	resp = term.Feed([]byte("\x1b[5n"))
	if string(resp) != "\x1b[0n" {
		t.Fatalf("DSR 5n: got %q, want ESC[0n", resp)
	}
}

func TestDECCharset(t *testing.T) {
	term := NewTerm(20, 5)
	term.Feed([]byte("\x1b(0"))
	term.Feed([]byte("qlx"))
	// q -> ─, l -> ┌, x -> │ in DEC graphics
	line := term.LineText(term.buf, 0)
	if r := []rune(line); len(r) < 3 || string(r[:3]) != "─┌│" {
		t.Fatalf("DEC charset: got %q, want ─┌│ prefix", line)
	}
	term.Feed([]byte("\x1b(B"))
	term.Feed([]byte("q"))
	// After ESC(B, q is literal q, at column 3
	if term.buf[0][3].Ch != 'q' {
		t.Fatalf("ASCII after ESC(B: got %q", term.buf[0][3].Ch)
	}
}

func TestInsertModeAndICH(t *testing.T) {
	term := NewTerm(20, 5)
	term.Feed([]byte("ABCDE"))
	term.Feed([]byte("\x1b[1;1H")) // home, actually row 1 col 1 -> use H
	// Move cursor to col 2 (B): ESC[1;2H
	term.Feed([]byte("\x1b[1;2H"))
	term.Feed([]byte("\x1b[4h")) // IRM on
	term.Feed([]byte("X"))
	if got := term.LineText(term.buf, 0); got[:6] != "AXBCDE" {
		t.Fatalf("insert mode: got %q, want AXBCDE prefix", got)
	}
	term.Feed([]byte("\x1b[4l")) // IRM off
	term.Feed([]byte("\x1b[1;1H"))
	term.Feed([]byte("\x1b[2@")) // ICH 2 at col 1
	if got := term.LineText(term.buf, 0); got[:8] != "  AXBCDE" {
		t.Fatalf("ICH: got %q, want '  AXBCDE' prefix", got)
	}
}

func TestMode1048AndTabStops(t *testing.T) {
	term := NewTerm(20, 5)
	term.Feed([]byte("\x1b[3;5H")) // row 3, col 5
	term.Feed([]byte("\x1b[?1048h"))
	term.Feed([]byte("\x1b[1;1H"))
	term.Feed([]byte("\x1b[?1048l"))
	if term.CursorX != 4 || term.CursorY != 2 {
		t.Fatalf("1048 restore: got %d,%d want 4,2", term.CursorX, term.CursorY)
	}
	// HTS at col 3, then clear all, then Tab should go to last col
	term2 := NewTerm(20, 5)
	term2.Feed([]byte("\x1b[3g")) // clear all tab stops
	term2.Feed([]byte("\t"))
	if term2.CursorX != 19 {
		t.Fatalf("Tab with no stops: got %d want 19", term2.CursorX)
	}
	term2.Feed([]byte("\x1b[1;1H"))
	term2.Feed([]byte("\x1bH")) // HTS? Actually ESC H sets stop at col 0 — use cursor at col 4
	term2.Feed([]byte("\x1b[1;5H"))
	term2.Feed([]byte("\x1bH"))
	term2.Feed([]byte("\x1b[1;1H"))
	term2.Feed([]byte("\t"))
	if term2.CursorX != 4 {
		t.Fatalf("HTS Tab: got %d want 4", term2.CursorX)
	}
}

func TestSGRDimStrike(t *testing.T) {
	term := NewTerm(20, 5)
	term.Feed([]byte("\x1b[2m\x1b[9mX"))
	c := term.buf[0][0]
	if !c.Dim || !c.Strike {
		t.Fatalf("SGR 2+9: dim=%v strike=%v", c.Dim, c.Strike)
	}
	term.Feed([]byte("\x1b[22m\x1b[29mY"))
	c2 := term.buf[0][1]
	if c2.Dim || c2.Strike {
		t.Fatalf("SGR 22+29 must clear: dim=%v strike=%v", c2.Dim, c2.Strike)
	}
	term.Feed([]byte("\x1b[mZ"))
	c3 := term.buf[0][2]
	if c3.Dim || c3.Strike {
		t.Fatalf("SGR 0 must clear dim/strike")
	}
}
