// Terminal emulator (VT100/xterm subset) — pure Go, no Windows dependency,
// so it can be unit-tested on any platform. The Windows UI only renders
// the screen buffer this produces.
package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Cell is one screen cell. Colours are 0xRRGGBB; Default flags mean
// "use the UI default fg/bg" (white on black).
type Cell struct {
	Ch        rune
	Fg        uint32
	Bg        uint32
	FgDefault bool
	BgDefault bool
	Bold      bool
	Reverse   bool
	Underline bool
	Dim       bool
	Strike    bool
}

func blankCell() Cell { return Cell{Ch: ' ', FgDefault: true, BgDefault: true} }

func blankLine(cols int) []Cell {
	l := make([]Cell, cols)
	for i := range l {
		l[i] = blankCell()
	}
	return l
}

func newBuffer(cols, rows int) [][]Cell {
	b := make([][]Cell, rows)
	for i := range b {
		b[i] = blankLine(cols)
	}
	return b
}

// Palet ala PuTTY (bukan Termux pekat lagi, atas permintaan user
// 2026-10-29 yang membandingkan screenshot dengan PuTTY): warna normal
// = 187 (BB), warna bright = 255/85. Teks normal jadi abu-abu terang,
// bukan putih pekat — itulah yang membuat tampilan PuTTY terasa lebih
// lembut, sementara teks tebal/bright tetap putih pekat.
var palette16 = []uint32{
	0x000000, 0xBB0000, 0x00BB00, 0xBBBB00,
	0x0000BB, 0xBB00BB, 0x00BBBB, 0xBBBBBB,
	0x555555, 0xFF5555, 0x55FF55, 0xFFFF55,
	0x5555FF, 0xFF55FF, 0x55FFFF, 0xFFFFFF,
}

// Warna kursor (blok kedip) — hijau saja, tidak mengikuti karakter
// di bawahnya. Sebelumnya kursor hanya menukar fg/bg sel di bawahnya,
// sehingga warnanya berubah-ubah: hijau di atas prompt hijau, biru di
// atas direktori biru, abu-abu di atas teks biasa, dan kadang hijau
// di atas spasi — itulah yang dilaporkan user 2026-10-29.
// Sekarang kursor selalu blok hijau dengan teks hitam, kedip bergantian
// hijau muda (bright, palet PuTTY 10) dan hijau tua (normal, palet
// PuTTY 2), apa pun karakter/spasi di bawahnya, termasuk di atas teks
// reverse/seleksi (kursor menang atas keduanya).
func cursorColors(blinkOn bool) (fg, bg uint32) {
	if blinkOn {
		return 0x000000, 0x55FF55 // hijau muda
	}
	return 0x000000, 0x00BB00 // hijau tua
}

func ansi256(n int) uint32 {
	if n < 0 {
		return 0xBBBBBB
	}
	if n < 16 {
		return palette16[n]
	}
	if n < 232 {
		n -= 16
		r := n / 36
		g := (n / 6) % 6
		b := n % 6
		v := func(x int) uint32 {
			if x == 0 {
				return 0
			}
			return uint32(55 + 40*x)
		}
		return v(r)<<16 | v(g)<<8 | v(b)
	}
	v := uint32(8 + 10*(n-232))
	return v<<16 | v<<8 | v
}

type Term struct {
	Cols, Rows int
	buf        [][]Cell
	alt        [][]Cell
	altActive  bool
	scrollback [][]Cell
	CursorX    int
	CursorY    int
	CursorVis  bool
	savedX     int
	savedY     int
	savedAltX  int
	savedAltY  int
	top        int // scroll region top (inclusive)
	bottom     int // scroll region bottom (inclusive)
	wrapPend   bool
	autoWrap   bool
	// wrapStartY: baris pertama dari baris logis yang sedang terpotong
	// (wrapped) karena auto-wrap, atau -1 kalau tidak ada. Dipakai untuk
	// bug progress bar 2026-10-29: bar progress (mis. apt/update-initramfs
	// "Progress: [ 99%] [####...]") yang panjangnya melebihi lebar terminal
	// terpotong menjadi 2+ baris fisik. Tanpa ini, \r sesudah bar tersebut
	// hanya kembali ke awal baris fisik terakhir (fragmen), sehingga update
	// bar berikutnya menimpa fragmen itu lalu terpotong lagi ke baris baru
	// — satu baris baru per update, persis seperti di screenshot user
	// (puluhan baris "Progress: [ 99%]" yang sama). Dengan ini, \r kembali
	// ke awal baris logis, jadi bar berikutnya menimpa baris logis yang
	// sama (terpotong lagi ke fragmen yang sama) dan tidak pernah menambah
	// baris baru. Hanya berlaku untuk \r yang langsung mengikuti bar
	// terpotong tanpa pergerakan kursor/newline di antaranya; semua
	// pergerakan kursor, newline, insert/delete, erase display, resize,
	// dan ganti alt screen menghapusnya (fallback ke \r fisik standar).
	wrapStartY int
	// crLogical: true kalau \r terakhir adalah \r logis (ke wrapStartY)
	// dan belum ada karakter tercetak sesudahnya. Dipakai supaya urutan
	// \r\n sesudah baris terpotong tetap benar: \r logis + \n harus
	// mendarat di baris sesudah fragmen terakhir baris logis itu, bukan
	// di baris sesudah baris pertama baris logis itu. crLogicalEndY
	// menyimpan baris fragmen terakhir sebelum \r logis itu.
	crLogical    bool
	crLogicalEndY int
	// current attributes
	fg        uint32
	bg        uint32
	fgDef     bool
	bgDef     bool
	bold      bool
	reverse   bool
	underline bool
	dim       bool
	strike    bool
	insertMode bool
	// DEC graphics charset (perbaikan 2026-10-29): 0 = ASCII,
	// 1 = DEC special graphics. charsetG0/G1 adalah charset yang
	// ditetapkan ESC ( X / ESC ) X, charsetActive 0 = G0, 1 = G1
	// (dipilih SI 0x0F / SO 0x0E). Sebelumnya ESC ( 0 diabaikan,
	// jadi aplikasi lama yang pakai ACS tampil huruf, bukan garis.
	charsetG0     int
	charsetG1     int
	charsetActive int
	charsetNext   int // 0 = G0, 1 = G1, saat parser di stCharset
	// Tab stops kustom (HTS ESC H, TBC CSI g): kalau kosong,
	// default tiap 8 kolom. Disimpan per kolom.
	tabStops []bool
	// PromptRecolor: kalau true, baris prompt format user@host:dir#/$
	// diwarnai di sisi klien (user@host: hijau, direktori biru) tanpa
	// mengirim perintah apa pun ke server. Ini yang membuat prompt
	// hijau tanpa prompt ganda: tidak ada perintah PS1 yang dieksekusi
	// shell, jadi tidak ada prompt tambahan.
	PromptRecolor bool
	// AppCursor: DECCKM (CSI ? 1 h) — application cursor keys.
	// Kalau true, panah/Home/End dikirim sebagai SS3 (ESC O …),
	// bukan CSI (ESC [ …), seperti PuTTY. Sebelum perbaikan
	// 2026-10-29 mode ini tidak dilacak sama sekali, jadi KERM
	// selalu mengirim CSI walau aplikasi (vim/htop) meminta SS3.
	AppCursor bool
	// MouseMode: mouse tracking yang diminta aplikasi —
	// 0 = mati, 1 = klik saja (1000), 2 = klik+drag (1002),
	// 3 = semua gerakan (1003). MouseSGR: mode SGR (1006).
	// BracketedPaste: mode 2004 — paste dibungkus ESC[200~…ESC[201~.
	// Ketiganya dilacak mulai 2026-10-29 agar KERM sama dengan PuTTY;
	// sebelumnya mode-mode ini diabaikan total.
	MouseMode     int
	MouseSGR      bool
	BracketedPaste bool
	// parser state
	state      int
	csiParams  []byte
	csiPrivate bool
	oscBuf     strings.Builder
}

const (
	stGround = iota
	stEsc
	stCSI
	stOSC
	stCharset
	stDCS
)

func NewTerm(cols, rows int) *Term {
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}
	t := &Term{
		Cols: cols, Rows: rows,
		buf: newBuffer(cols, rows), alt: newBuffer(cols, rows),
		CursorVis: true, autoWrap: true,
		fgDef: true, bgDef: true,
		top: 0, bottom: rows - 1,
		wrapStartY: -1,
	}
	t.tabStops = defaultTabStops(cols)
	return t
}

func (t *Term) Resize(cols, rows int) {
	if cols < 1 || rows < 1 || (cols == t.Cols && rows == t.Rows) {
		return
	}
	resizeBuf := func(b [][]Cell) [][]Cell {
		nb := newBuffer(cols, rows)
		for y := 0; y < rows && y < len(b); y++ {
			for x := 0; x < cols && x < len(b[y]); x++ {
				nb[y][x] = b[y][x]
			}
		}
		return nb
	}
	t.buf = resizeBuf(t.buf)
	t.alt = resizeBuf(t.alt)
	// Tab stops: pertahankan yang lama, kolom baru pakai default.
	nt := defaultTabStops(cols)
	for i := 0; i < len(t.tabStops) && i < len(nt); i++ {
		nt[i] = t.tabStops[i]
	}
	t.tabStops = nt
	t.Cols, t.Rows = cols, rows
	if t.CursorX >= cols {
		t.CursorX = cols - 1
	}
	if t.CursorY >= rows {
		t.CursorY = rows - 1
	}
	t.top = 0
	t.bottom = rows - 1
	t.wrapPend = false
	t.wrapStartY = -1
	t.crLogical = false
}

func defaultTabStops(cols int) []bool {
	s := make([]bool, cols)
	for i := range s {
		if i > 0 && i%8 == 0 {
			s[i] = true
		}
	}
	return s
}

// acsMap: DEC special graphics -> Unicode box drawing, sesuai xterm.
// Dipakai putChar kalau charset aktif = DEC (1).
var acsMap = map[rune]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊',
	'f': '°', 'g': '±', 'h': '␤', 'i': '␋', 'j': '┘', 'k': '┐',
	'l': '┌', 'm': '└', 'n': '┼', 'o': '⎺', 'p': '⎻', 'q': '─',
	'r': '⎼', 's': '⎽', 't': '├', 'u': '┤', 'v': '┴', 'w': '┬',
	'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠', '}': '£',
	'~': '·',
}

func (t *Term) activeCharset() int {
	if t.charsetActive == 1 {
		return t.charsetG1
	}
	return t.charsetG0
}

func (t *Term) curBuf() [][]Cell {
	if t.altActive {
		return t.alt
	}
	return t.buf
}

func (t *Term) ScrollbackLen() int { return len(t.scrollback) }

func (t *Term) AltActive() bool { return t.altActive }

// LinesForView returns Rows lines for a given scroll offset.
// offset 0 = live screen; offset > 0 = scrolled up into scrollback.
// Alt screen has no scrollback.
func (t *Term) LinesForView(offset int) [][]Cell {
	if t.altActive || offset <= 0 {
		return t.curBuf()
	}
	total := append(append([][]Cell{}, t.scrollback...), t.buf...)
	if offset > len(t.scrollback) {
		offset = len(t.scrollback)
	}
	start := len(total) - t.Rows - offset
	out := make([][]Cell, t.Rows)
	for i := 0; i < t.Rows; i++ {
		if start+i >= 0 && start+i < len(total) {
			out[i] = total[start+i]
		} else {
			out[i] = blankLine(t.Cols)
		}
	}
	return out
}

func (t *Term) scrollUp(n int) {
	if n < 1 {
		n = 1
	}
	buf := t.curBuf()
	for i := 0; i < n; i++ {
		if !t.altActive && t.top == 0 && t.bottom == t.Rows-1 {
			t.scrollback = append(t.scrollback, append([]Cell{}, buf[0]...))
			if len(t.scrollback) > 2000 {
				t.scrollback = t.scrollback[len(t.scrollback)-2000:]
			}
		}
		copy(buf[t.top:t.bottom], buf[t.top+1:t.bottom+1])
		buf[t.bottom] = blankLine(t.Cols)
		// Baris logis yang terpotong ikut bergeser ke atas bersama
		// buffer; kalau baris pertamanya keluar dari region (scroll
		// off), pelacakan wrap tidak lagi valid.
		if t.wrapStartY >= t.top && t.wrapStartY <= t.bottom {
			t.wrapStartY--
			if t.wrapStartY < t.top {
				t.wrapStartY = -1
			}
		}
	}
}

func (t *Term) scrollDown(n int) {
	if n < 1 {
		n = 1
	}
	buf := t.curBuf()
	for i := 0; i < n; i++ {
		copy(buf[t.top+1:t.bottom+1], buf[t.top:t.bottom])
		buf[t.top] = blankLine(t.Cols)
		if t.wrapStartY >= t.top && t.wrapStartY <= t.bottom {
			t.wrapStartY++
			if t.wrapStartY > t.bottom {
				t.wrapStartY = -1
			}
		}
	}
}

func (t *Term) lineFeed() {
	if t.CursorY == t.bottom {
		t.scrollUp(1)
	} else if t.CursorY < t.Rows-1 {
		t.CursorY++
	}
	t.wrapPend = false
}

func (t *Term) putChar(r rune) {
	if r == 0 {
		return
	}
	// Karakter tercetak mengakhiri urutan \r logis + \n: \n sesudah
	// karakter ini harus dari baris fisik saat ini, bukan dari fragmen
	// baris logis lama (lihat \r/\n di Feed).
	t.crLogical = false
	if t.wrapPend {
		t.CursorX = 0
		t.lineFeed()
		// Baris baru ini adalah kelanjutan baris logis yang sama.
		// Kalau rantai wrap sudah ada (baris logis terpotong lebih
		// dari sekali), pertahankan awalnya — sudah disesuaikan oleh
		// scrollUp kalau lineFeed tadi men-scroll. Kalau ini wrap
		// pertama dari baris logis ini, awalnya adalah baris sebelum
		// fragmen ini (CursorY-1; sesudah scroll, baris pertama itu
		// sudah bergeser ke CursorY-1 juga).
		if t.wrapStartY < 0 {
			if t.CursorY > 0 {
				t.wrapStartY = t.CursorY - 1
			} else {
				t.wrapStartY = -1
			}
		}
	}
	if t.CursorY < 0 || t.CursorY >= t.Rows || t.CursorX < 0 || t.CursorX >= t.Cols {
		return
	}
	if t.activeCharset() == 1 {
		if m, ok := acsMap[r]; ok {
			r = m
		}
	}
	buf := t.curBuf()
	// Insert mode (IRM): geser sel ke kanan sebelum menulis,
	// bukan menimpa - seperti PuTTY. Sel terakhir baris hilang.
	if t.insertMode && t.CursorX < t.Cols-1 {
		copy(buf[t.CursorY][t.CursorX+1:], buf[t.CursorY][t.CursorX:t.Cols-1])
	}
	c := Cell{Ch: r, Fg: t.fg, Bg: t.bg, FgDefault: t.fgDef, BgDefault: t.bgDef,
		Bold: t.bold, Reverse: t.reverse, Underline: t.underline, Dim: t.dim, Strike: t.strike}
	buf[t.CursorY][t.CursorX] = c
	if t.CursorX == t.Cols-1 {
		if t.autoWrap {
			t.wrapPend = true
		}
	} else {
		t.CursorX++
	}
}

func (t *Term) eraseInLine(mode int) {
	if t.CursorY < 0 || t.CursorY >= t.Rows {
		return
	}
	buf := t.curBuf()
	switch mode {
	case 0:
		for x := t.CursorX; x < t.Cols; x++ {
			buf[t.CursorY][x] = blankCell()
		}
	case 1:
		for x := 0; x <= t.CursorX && x < t.Cols; x++ {
			buf[t.CursorY][x] = blankCell()
		}
	case 2:
		buf[t.CursorY] = blankLine(t.Cols)
	}
}

func (t *Term) eraseInDisplay(mode int) {
	buf := t.curBuf()
	switch mode {
	case 0:
		t.eraseInLine(0)
		for y := t.CursorY + 1; y < t.Rows; y++ {
			buf[y] = blankLine(t.Cols)
		}
	case 1:
		for y := 0; y < t.CursorY; y++ {
			buf[y] = blankLine(t.Cols)
		}
		t.eraseInLine(1)
	case 2, 3:
		for y := 0; y < t.Rows; y++ {
			buf[y] = blankLine(t.Cols)
		}
		if mode == 3 {
			t.scrollback = nil
		}
	}
}

func (t *Term) setSGR(params []int) {
	if len(params) == 0 {
		params = []int{0}
	}
	// BUG nano 2026-10-29: parameter kosong di CSI SGR (parseCSIParams
	// mengembalikan -1 untuk param kosong) dulu diabaikan, padahal
	// menurut ECMA-48 param kosong = 0 (reset). Akibatnya reset tanpa
	// parameter (\x1b[m) — bagian dari sgr0 terminfo "\E(B\E[m" yang
	// dikirim nano/curses saat keluar, dan juga dipakai untuk reset
	// tiap baris di dalam nano — tidak pernah mereset reverse/bg.
	// Reverse/background dari status bar nano lalu bocor ke teks
	// berikutnya di dalam nano, dan ke prompt shell setelah Ctrl+X.
	// Normalisasi -1 -> 0 di sini memperbaiki semua posisi kosong
	// (awal, tengah, maupun reset tunggal), bukan hanya kasus tunggal.
	for i, p := range params {
		if p == -1 {
			params[i] = 0
		}
	}
	for i := 0; i < len(params); i++ {
		p := params[i]
		switch {
		case p == 0:
			t.fgDef, t.bgDef, t.bold, t.reverse, t.underline, t.dim, t.strike = true, true, false, false, false, false, false
			t.fg, t.bg = 0, 0
		case p == 1:
			t.bold = true
			if t.fgDef {
				// bold on default fg is rendered bright by the UI
			}
		case p == 2:
			// Faint/dim - sebelumnya salah diartikan sebagai bold off.
			// Bold off yang benar adalah SGR 22.
			t.dim = true
		case p == 9:
			t.strike = true
		case p == 3:
			// italic: not rendered distinctly
		case p == 4:
			t.underline = true
		case p == 7:
			t.reverse = true
		case p == 22:
			t.bold = false
			t.dim = false
		case p == 24:
			t.underline = false
		case p == 29:
			t.strike = false
		case p == 27:
			t.reverse = false
		case p == 39:
			t.fgDef = true
		case p == 49:
			t.bgDef = true
		case p >= 30 && p <= 37:
			t.fg, t.fgDef = ansi256(p-30), false
		case p >= 40 && p <= 47:
			t.bg, t.bgDef = ansi256(p-40), false
		case p >= 90 && p <= 97:
			t.fg, t.fgDef = ansi256(p-90+8), false
		case p >= 100 && p <= 107:
			t.bg, t.bgDef = ansi256(p-100+8), false
		case (p == 38 || p == 48) && i+2 < len(params) && params[i+1] == 5:
			c := ansi256(params[i+2])
			if p == 38 {
				t.fg, t.fgDef = c, false
			} else {
				t.bg, t.bgDef = c, false
			}
			i += 2
		case (p == 38 || p == 48) && i+4 < len(params) && params[i+1] == 2:
			c := uint32(params[i+2])<<16 | uint32(params[i+3])<<8 | uint32(params[i+4])
			if p == 38 {
				t.fg, t.fgDef = c, false
			} else {
				t.bg, t.bgDef = c, false
			}
			i += 4
		}
	}
}

func (t *Term) handleCSI(final byte, params []int, priv bool) {
	p := func(i, def int) int {
		if i < len(params) && params[i] != -1 {
			return params[i]
		}
		return def
	}
	if priv {
		switch final {
		case 'h', 'l':
			enable := final == 'h'
			for _, m := range params {
				switch m {
				case 1:
					t.AppCursor = enable
				case 25:
					t.CursorVis = enable
				case 7:
					t.autoWrap = enable
				case 1000:
					if enable {
						t.MouseMode = 1
					} else if t.MouseMode == 1 {
						t.MouseMode = 0
					}
				case 1002:
					if enable {
						t.MouseMode = 2
					} else if t.MouseMode == 2 {
						t.MouseMode = 0
					}
				case 1003:
					if enable {
						t.MouseMode = 3
					} else if t.MouseMode == 3 {
						t.MouseMode = 0
					}
				case 1048:
					// Save/restore cursor seperti DECSC/DECRC (ESC 7/8),
					// dipakai aplikasi yang pakai 1047+1048, bukan 1049.
					if enable {
						t.savedX, t.savedY = t.CursorX, t.CursorY
					} else {
						t.CursorX, t.CursorY = t.savedX, t.savedY
					}
				case 1006:
					t.MouseSGR = enable
				case 2004:
					t.BracketedPaste = enable
				case 1047, 1049:
					// Ganti buffer (alt/main): pelacakan wrap baris
					// lama tidak berlaku untuk buffer yang baru.
					t.wrapStartY = -1
					t.crLogical = false
					if m == 1047 {
						if enable && !t.altActive {
							t.altActive = true
						} else if !enable && t.altActive {
							t.altActive = false
						}
					} else {
						if enable && !t.altActive {
							t.savedAltX, t.savedAltY = t.CursorX, t.CursorY
							t.alt = newBuffer(t.Cols, t.Rows)
							t.altActive = true
							t.CursorX, t.CursorY = 0, 0
						} else if !enable && t.altActive {
							t.altActive = false
							t.CursorX, t.CursorY = t.savedAltX, t.savedAltY
						}
					}
				}
			}
		}
		return
	}
	switch final {
	case '@':
		// ICH: sisipkan n sel kosong di kursor, geser ke kanan.
		n := p(0, 1)
		if t.CursorY >= 0 && t.CursorY < t.Rows && t.CursorX >= 0 && t.CursorX < t.Cols {
			buf := t.curBuf()
			for i := 0; i < n && t.CursorX < t.Cols; i++ {
				copy(buf[t.CursorY][t.CursorX+1:], buf[t.CursorY][t.CursorX:t.Cols-1])
				buf[t.CursorY][t.CursorX] = blankCell()
			}
		}
	case 'g':
		// TBC: 0 = hapus tab stop di kursor, 3 = hapus semua.
		if p(0, 0) == 3 {
			for i := range t.tabStops {
				t.tabStops[i] = false
			}
		} else if t.CursorX >= 0 && t.CursorX < len(t.tabStops) {
			t.tabStops[t.CursorX] = false
		}
	case 'h', 'l':
		// SM/RM non-privat: hanya mode 4 (IRM, insert mode) yang
		// dilacak; mode lain (mis. 20) diabaikan seperti sebelumnya.
		for _, m := range params {
			if m == 4 {
				t.insertMode = final == 'h'
			}
		}
	case 'A':
		t.CursorY -= p(0, 1)
		if t.CursorY < 0 {
			t.CursorY = 0
		}
	case 'B':
		t.CursorY += p(0, 1)
		if t.CursorY >= t.Rows {
			t.CursorY = t.Rows - 1
		}
	case 'C':
		t.CursorX += p(0, 1)
		if t.CursorX >= t.Cols {
			t.CursorX = t.Cols - 1
		}
	case 'D':
		t.CursorX -= p(0, 1)
		if t.CursorX < 0 {
			t.CursorX = 0
		}
	case 'E':
		t.CursorY += p(0, 1)
		if t.CursorY >= t.Rows {
			t.CursorY = t.Rows - 1
		}
		t.CursorX = 0
	case 'F':
		t.CursorY -= p(0, 1)
		if t.CursorY < 0 {
			t.CursorY = 0
		}
		t.CursorX = 0
	case 'G':
		t.CursorX = p(0, 1) - 1
		if t.CursorX < 0 {
			t.CursorX = 0
		}
		if t.CursorX >= t.Cols {
			t.CursorX = t.Cols - 1
		}
	case 'H', 'f':
		t.CursorY = p(0, 1) - 1
		t.CursorX = p(1, 1) - 1
		if t.CursorY < 0 {
			t.CursorY = 0
		}
		if t.CursorY >= t.Rows {
			t.CursorY = t.Rows - 1
		}
		if t.CursorX < 0 {
			t.CursorX = 0
		}
		if t.CursorX >= t.Cols {
			t.CursorX = t.Cols - 1
		}
	case 'J':
		t.eraseInDisplay(p(0, 0))
	case 'K':
		t.eraseInLine(p(0, 0))
	case 'L':
		n := p(0, 1)
		buf := t.curBuf()
		for i := 0; i < n; i++ {
			if t.CursorY >= t.top && t.CursorY <= t.bottom {
				copy(buf[t.CursorY+1:t.bottom+1], buf[t.CursorY:t.bottom])
				buf[t.CursorY] = blankLine(t.Cols)
			}
		}
	case 'M':
		n := p(0, 1)
		buf := t.curBuf()
		for i := 0; i < n; i++ {
			if t.CursorY >= t.top && t.CursorY <= t.bottom {
				copy(buf[t.CursorY:t.bottom], buf[t.CursorY+1:t.bottom+1])
				buf[t.bottom] = blankLine(t.Cols)
			}
		}
	case 'P':
		n := p(0, 1)
		if t.CursorY >= 0 && t.CursorY < t.Rows {
			buf := t.curBuf()
			for i := 0; i < n; i++ {
				copy(buf[t.CursorY][t.CursorX:], buf[t.CursorY][t.CursorX+1:])
				if t.Cols > 0 {
					buf[t.CursorY][t.Cols-1] = blankCell()
				}
			}
		}
	case 'S':
		t.scrollUp(p(0, 1))
	case 'T':
		t.scrollDown(p(0, 1))
	case 'X':
		n := p(0, 1)
		if t.CursorY >= 0 && t.CursorY < t.Rows {
			buf := t.curBuf()
			for i := 0; i < n && t.CursorX+i < t.Cols; i++ {
				buf[t.CursorY][t.CursorX+i] = blankCell()
			}
		}
	case 'd':
		t.CursorY = p(0, 1) - 1
		if t.CursorY < 0 {
			t.CursorY = 0
		}
		if t.CursorY >= t.Rows {
			t.CursorY = t.Rows - 1
		}
	case 'm':
		t.setSGR(params)
	case 'r':
		top := p(0, 1) - 1
		bottom := p(1, t.Rows) - 1
		if top < 0 {
			top = 0
		}
		if bottom >= t.Rows {
			bottom = t.Rows - 1
		}
		if top <= bottom {
			t.top, t.bottom = top, bottom
			t.CursorX, t.CursorY = 0, 0
		}
	case 's':
		t.savedX, t.savedY = t.CursorX, t.CursorY
	case 'u':
		t.CursorX, t.CursorY = t.savedX, t.savedY
	}
	t.wrapPend = false
	// Pergerakan kursor / scroll / insert-delete / erase display /
	// save-restore / scroll region mengakhiri baris logis yang sedang
	// terpotong: \r sesudahnya harus fisik standar, bukan logis.
	// Pengecualian: SGR (m) dan erase line (K) tidak menggerakkan
	// kursor — urutan progress "\r\x1b[Kbar baru" dan "\r\x1b[0mbar
	// baru" harus tetap bisa memakai \r logis.
	if final != 'm' && final != 'K' {
		t.wrapStartY = -1
		t.crLogical = false
	}
}

func parseCSIParams(b []byte) ([]int, bool) {
	priv := false
	if len(b) > 0 && b[0] == '?' {
		priv = true
		b = b[1:]
	}
	if len(b) == 0 {
		return []int{-1}, priv
	}
	parts := strings.Split(string(b), ";")
	out := make([]int, 0, len(parts))
	for _, s := range parts {
		if s == "" {
			out = append(out, -1)
			continue
		}
		v := 0
		ok := true
		for _, c := range s {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			v = v*10 + int(c-'0')
		}
		if !ok {
			out = append(out, -1)
		} else {
			out = append(out, v)
		}
	}
	return out, priv
}

// promptSpans mendeteksi prompt shell format standar user@host:dir#/ $
// di awal baris. Mengembalikan indeks (dalam rune) akhir segmen
// user@host: (termasuk titik dua) dan indeks karakter '#'/ '$' (awal
// segmen direktori berakhir di sana). ok=false kalau baris bukan prompt
// format itu (mis. banner "user@host: SSH terhubung" yang tidak punya
// #/$, atau prompt berkurung "[user@host dir]# ").
func promptSpans(line []rune) (userHostEnd, dirEnd int, ok bool) {
	isName := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-'
	}
	at := -1
	for i, r := range line {
		if r == ' ' {
			break
		}
		if r == '@' {
			at = i
			break
		}
		if !isName(r) {
			return 0, 0, false
		}
	}
	if at <= 0 {
		return 0, 0, false
	}
	colon := -1
	for i := at + 1; i < len(line); i++ {
		if line[i] == ' ' {
			break
		}
		if line[i] == ':' {
			colon = i
			break
		}
		if !isName(line[i]) {
			return 0, 0, false
		}
	}
	if colon <= at+1 {
		return 0, 0, false
	}
	// Direktori: dari colon+1 sampai #/ $, tanpa spasi di tengah.
	for i := colon + 1; i < len(line); i++ {
		if line[i] == '#' || line[i] == '$' {
			if i == colon+1 {
				return 0, 0, false
			}
			return colon + 1, i, true
		}
		if line[i] == ' ' {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// PromptDir: direktori dari prompt shell saat ini (user@host:dir#/ $),
// atau "" kalau baris kursor bukan prompt standar / alt screen aktif.
// Dipakai "SFTP here": jendela SFTP dibuka di direktori yang sedang
// tampil di prompt, bukan menebak dari tempat lain. "~" dikembalikan
// apa adanya; penerjemahan ke home absolut dilakukan resolvePromptDir.
func (t *Term) PromptDir() string {
	if t.altActive {
		return ""
	}
	if t.CursorY < 0 || t.CursorY >= len(t.buf) {
		return ""
	}
	row := t.buf[t.CursorY]
	line := make([]rune, len(row))
	for i, c := range row {
		line[i] = c.Ch
	}
	for len(line) > 0 && line[len(line)-1] == ' ' {
		line = line[:len(line)-1]
	}
	userHostEnd, dirEnd, ok := promptSpans(line)
	if !ok {
		return ""
	}
	return string(line[userHostEnd:dirEnd])
}

// recolorPromptLine mewarnai ulang baris prompt di kursor (buffer utama
// saja, bukan alt screen) kalau PromptRecolor aktif: user@host: hijau
// tebal (palet PuTTY bright green), direktori biru tebal (bright blue),
// karakter #/ $ dan perintah sesudahnya dibiarkan apa adanya.
func (t *Term) recolorPromptLine() {
	if !t.PromptRecolor || t.altActive {
		return
	}
	if t.CursorY < 0 || t.CursorY >= len(t.buf) {
		return
	}
	row := t.buf[t.CursorY]
	line := make([]rune, len(row))
	for i, c := range row {
		line[i] = c.Ch
	}
	// Buang spasi kosong di akhir baris untuk deteksi (prompt "root@h:~# "
	// di buffer bisa berakhir di '#' saja setelah trim, tetap terdeteksi).
	for len(line) > 0 && line[len(line)-1] == ' ' {
		line = line[:len(line)-1]
	}
	userHostEnd, dirEnd, ok := promptSpans(line)
	if !ok {
		return
	}
	// Pertahanan kedua (bug nano): segmen prompt harus selalu tampil
	// sebagai teks berwarna di atas hitam default, bukan teks hitam
	// di atas background berwarna. Kalau reverse sempat bocor ke
	// baris prompt (mis. reset terlewat dari aplikasi sebelumnya),
	// cellColors akan menukar fg/bg sehingga hijau/biru yang
	// dimaksud sebagai warna teks justru menjadi warna background.
	// Jadi paksa Reverse=false dan background default untuk segmen
	// prompt, di samping Fg/Bold yang sudah ada.
	for i := 0; i < userHostEnd && i < len(row); i++ {
		row[i].Fg = 0x55FF55
		row[i].Bold = true
		row[i].FgDefault = false
		row[i].Reverse = false
		row[i].BgDefault = true
		row[i].Bg = 0
	}
	for i := userHostEnd; i < dirEnd && i < len(row); i++ {
		row[i].Fg = 0x5555FF
		row[i].Bold = true
		row[i].FgDefault = false
		row[i].Reverse = false
		row[i].BgDefault = true
		row[i].Bg = 0
	}
}

// Feed processes output from the SSH server. It returns bytes that should

// be sent back to the server (e.g. answers to Device Attributes queries).
func (t *Term) Feed(data []byte) []byte {
	var resp []byte
	i := 0
	for i < len(data) {
		b := data[i]
		switch t.state {
		case stGround:
			switch b {
			case 0x1b:
				t.state = stEsc
				i++
			case '\r':
				if t.wrapStartY >= 0 && t.CursorY != t.wrapStartY {
					// \r logis (bug progress bar): kembali ke awal
					// baris logis yang terpotong, bukan ke awal
					// fragmen terakhirnya. Ingat fragmen terakhir
					// untuk kasus \r\n (lihat \n di bawah).
					t.crLogical = true
					t.crLogicalEndY = t.CursorY
					t.CursorY = t.wrapStartY
				} else {
					t.crLogical = false
				}
				t.CursorX = 0
				t.wrapPend = false
				t.wrapStartY = -1
				i++
			case '\n', 0x0b, 0x0c:
				if t.crLogical {
					// \r\n sesudah baris terpotong: \n harus dari
					// fragmen terakhir baris logis itu, jadi mendarat
					// di baris sesudah baris logis itu, bukan di
					// baris sesudah baris pertamanya.
					t.CursorY = t.crLogicalEndY
					t.crLogical = false
				}
				t.lineFeed()
				t.wrapStartY = -1
				i++
			case '\b':
				if t.CursorX > 0 {
					t.CursorX--
				}
				t.wrapPend = false
				i++
			case '\t':
				// Tab stop kustom kalau ada, kalau tidak default tiap 8.
				nx := t.Cols - 1
				for x := t.CursorX + 1; x < t.Cols; x++ {
					if x < len(t.tabStops) && t.tabStops[x] {
						nx = x
						break
					}
				}
				t.CursorX = nx
				t.wrapPend = false
				// Tab menggerakkan kursor di baris saat ini: urutan
				// \r\n tidak lagi berlaku (lihat \r logis).
				t.crLogical = false
				i++
			case 0x0E:
				// SO: aktifkan G1 (charset kedua).
				t.charsetActive = 1
				i++
			case 0x0F:
				// SI: aktifkan G0 (charset pertama).
				t.charsetActive = 0
				i++
			case 0x07:
				i++ // bell: ignore
			default:
				if b < 0x20 {
					i++
					continue
				}
				r, size := utf8.DecodeRune(data[i:])
				if r == utf8.RuneError && size == 1 {
					r = '�'
				}
				t.putChar(r)
				i += size
			}
		case stEsc:
			switch b {
			case '[':
				t.state = stCSI
				t.csiParams = t.csiParams[:0]
				t.csiPrivate = false
			case ']':
				t.state = stOSC
				t.oscBuf.Reset()
			case '(':
				t.charsetNext = 0
				t.state = stCharset
			case ')':
				t.charsetNext = 1
				t.state = stCharset
			case 'H':
				// HTS: set tab stop di kolom kursor.
				if t.CursorX >= 0 && t.CursorX < len(t.tabStops) {
					t.tabStops[t.CursorX] = true
				}
				t.state = stGround
			case 'P':
				t.state = stDCS
			case '7':
				t.savedX, t.savedY = t.CursorX, t.CursorY
				t.wrapStartY = -1
				t.crLogical = false
				t.state = stGround
			case '8':
				t.CursorX, t.CursorY = t.savedX, t.savedY
				t.wrapStartY = -1
				t.crLogical = false
				t.state = stGround
			case 'c':
				*t = *NewTerm(t.Cols, t.Rows)
				t.state = stGround
			case 'D':
				t.lineFeed()
				t.wrapStartY = -1
				t.crLogical = false
				t.state = stGround
			case 'E':
				t.CursorX = 0
				t.lineFeed()
				t.wrapStartY = -1
				t.crLogical = false
				t.state = stGround
			case 'M':
				if t.CursorY == t.top {
					t.scrollDown(1)
				} else if t.CursorY > 0 {
					t.CursorY--
				}
				t.wrapStartY = -1
				t.crLogical = false
				t.state = stGround
			default:
				t.state = stGround
			}
			i++
		case stCSI:
			if b >= 0x30 && b <= 0x3f {
				t.csiParams = append(t.csiParams, b)
				i++
			} else if b >= 0x20 && b <= 0x2f {
				// intermediate byte: ignore
				i++
			} else if b >= 0x40 && b <= 0x7e {
				params, priv := parseCSIParams(t.csiParams)
				if b == 'c' && !priv {
					resp = append(resp, []byte("\x1b[?62;c")...)
				} else if b == 'n' && !priv {
					// DSR (Device Status Report), dijawab seperti
					// PuTTY mulai 2026-10-29; sebelumnya KERM diam
					// saja, jadi aplikasi yang menanyakan posisi
					// kursor tidak pernah mendapat jawaban.
					// 6 (atau tanpa parameter) = posisi kursor,
					// 5 = status perangkat (OK = 0).
					n := 6
					if len(params) > 0 && params[0] != -1 {
						n = params[0]
					}
					if n == 5 {
						resp = append(resp, []byte("\x1b[0n")...)
					} else if n == 6 {
						resp = append(resp, []byte(fmt.Sprintf("\x1b[%d;%dR", t.CursorY+1, t.CursorX+1))...)
					}
				} else {
					t.handleCSI(b, params, priv)
				}
				t.state = stGround
				i++
			} else {
				t.state = stGround
				i++
			}
		case stOSC:
			if b == 0x07 {
				t.state = stGround
			} else if b == 0x1b {
				// could be ST: consume next byte if '\'
				if i+1 < len(data) && data[i+1] == '\\' {
					i += 2
				} else {
					i++
				}
				t.state = stGround
			} else {
				i++
			}
		case stCharset:
			// '0' = DEC special graphics, lainnya = ASCII (termasuk
			// 'B' ASCII dan 'A' UK yang diperlakukan sebagai ASCII).
			cs := 0
			if b == '0' {
				cs = 1
			}
			if t.charsetNext == 1 {
				t.charsetG1 = cs
			} else {
				t.charsetG0 = cs
			}
			t.state = stGround
			i++
		case stDCS:
			if b == 0x1b && i+1 < len(data) && data[i+1] == '\\' {
				i += 2
				t.state = stGround
			} else {
				i++
			}
		}
	}
	// Prompt hijau klien: setelah output diproses, warnai ulang baris
	// prompt di kursor (kalau cocok). Idempotent: dipanggil tiap Feed,
	// hanya segmen prompt yang diubah, perintah yang diketik tidak.
	t.recolorPromptLine()
	return resp
}

// LineTextAbs returns the text of an absolute line (see
// selectionAbsLine in selection.go): 0 = oldest scrollback line,
// len(scrollback)+Rows-1 = bottom live line. Alt screen has no
// scrollback, so absolute = live row. Out of range returns "".
func (t *Term) LineTextAbs(absY int) string {
	if t.altActive {
		if absY < 0 || absY >= len(t.alt) {
			return ""
		}
		return t.LineText(t.alt, absY)
	}
	if absY < 0 {
		return ""
	}
	if absY < len(t.scrollback) {
		return t.LineText(t.scrollback, absY)
	}
	y := absY - len(t.scrollback)
	if y < 0 || y >= len(t.buf) {
		return ""
	}
	return t.LineText(t.buf, y)
}

// Text returns the visible text of a line (for clipboard copy).
func (t *Term) LineText(lines [][]Cell, y int) string {
	if y < 0 || y >= len(lines) {
		return ""
	}
	var sb strings.Builder
	for _, c := range lines[y] {
		if c.Ch == 0 {
			sb.WriteRune(' ')
		} else {
			sb.WriteRune(c.Ch)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}
