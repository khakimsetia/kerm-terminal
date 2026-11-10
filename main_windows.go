//go:build windows

// KERM Terminal — aplikasi desktop Windows asli (Win32, bukan browser).
// Jendela sesi ala PuTTY + jendela terminal dengan emulator VT sendiri.
package main

import (
	"encoding/json"
	"path/filepath"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

const (
	wmAppOutput      = win.WM_APP + 1
	wmAppClosed      = win.WM_APP + 2
	wmAppConfirm     = win.WM_APP + 3
	wmAppConnectDone = win.WM_APP + 4

	idHost = 100 + iota
	idPort
	idUser
	idAuthCombo
	idProtoCombo
	idAuthLabel
	idGreenPrompt
	idPassLabel
	idPass
	idKeyLabel
	idKeyPath
	idBrowse
	idKeyPassLabel
	idKeyPass
	idSessionName
	idSaveSession
	idDeleteSession
	idNewSession
	idSessionList
	idConnect
	idCancelConnect
	idSFTPButton
	idStatus
	idHeaderBanner
	idHeaderTitle
	idHeaderSub
)

var (
	hInst         win.HINSTANCE
	appIcon       win.HICON
	appIconSm     win.HICON
	hwndSession   win.HWND
	termFont      win.HFONT
	uiFont        win.HFONT
	uiFontBold    win.HFONT
	headerFont    win.HFONT
	headerSubFont win.HFONT
	sessionBgBrush win.HBRUSH
	accentBrush   win.HBRUSH
	termBgBrush   win.HBRUSH
	connectGen    int
	connectInProg bool
	ctlCancel     win.HWND
	fontEnumCallback uintptr
	charW, charH  int32
	connMu        sync.Mutex
	// Satu proses, 1 panel sesi, banyak terminal (perbaikan 2026-10-29):
	// tiap terminal punya state sendiri di termWin, dipetakan dari HWND.
	// Panel sesi ditutup (hide) saat terminal jalan, dan dibuka lagi
	// saat terminal ditutup — tanpa perlu proses baru, jadi panel
	// tidak pernah bertambah dan terminal bisa banyak.
	termWins    = map[win.HWND]*termWin{}
	// termOrder: urutan terminal dibuka (map tidak punya urutan,
	// iterasinya acak — pemilih SFTP harus tampil urut dibuka,
	// bukan acak, supaya duplikat user@host yang sama bisa dibedakan
	// dari posisinya).
	termOrder   []win.HWND
	pendingTerm *termWin
	// session controls
	ctlHost, ctlPort, ctlUser, ctlAuth, ctlProto, ctlGreenPrompt, ctlPass, ctlKeyPath, ctlKeyPass, ctlSessionName, ctlStatus, ctlList win.HWND
	sessions      []Session
	keyFileData   []byte
	keyFileLoaded string
	sessionSelectedViaKeys bool
)

// termWin adalah satu jendela terminal + koneksinya. Semua state yang
// dulu global (term, conn, scroll, seleksi, password prompt) kini
// per-terminal, supaya banyak terminal bisa jalan bersamaan dalam
// satu proses dengan satu panel sesi.
type termWin struct {
	hwnd           win.HWND
	term           *Term
	conn           *Conn
	cfg            Session
	outQueue       [][]byte
	outMu          sync.Mutex
	scrollOffset   int
	wheelRemainder int
	cursorBlinkOn  bool
	selActive      bool
	selStartX      int
	selStartAbsY   int
	selEndX        int
	selEndAbsY     int
	selDragging    bool
	selAutoScroll  bool
	// mouseTrackBtn: tombol mouse yang sedang ditekan dalam mode
	// mouse reporting (PuTTY 1000/1002/1003), -1 = tidak ada.
	// Dipakai supaya drag/release dikirim ke aplikasi, bukan
	// seleksi lokal — kecuali Shift ditekan (seleksi lokal).
	mouseTrackBtn  int
	pwActive       bool
	pwBuf          []rune
	pwCfg          Session
	// Disconnect tidak normal (2026-10-29): terminal TIDAK langsung
	// dihancurkan, melainkan tetap terbuka dengan banner keterangan,
	// supaya tidak hilang diam-diam saat laptop sleep / jaringan
	// putus. Field ini di-set sekali di wmAppClosed, lalu WM_CLOSE
	// biasa (tutup terminal -> panel) yang mengakhiri terminal ini.
	disconnected   bool
	disconnReason  string
	disconnDetail  string
	disconnTime    time.Time
}

func termByHWND(hwnd win.HWND) *termWin { return termWins[hwnd] }

// removeTermOrder: hapus HWND dari urutan dibuka (dipanggil saat
// terminal dihancurkan, supaya pemilih SFTP tidak menampilkan
// terminal yang sudah tidak ada).
func removeTermOrder(hwnd win.HWND) {
	for i, h := range termOrder {
		if h == hwnd {
			termOrder = append(termOrder[:i], termOrder[i+1:]...)
			return
		}
	}
}


func destroyTermWindow(t *termWin) {
	if t == nil || t.hwnd == 0 {
		return
	}
	// SFTP milik terminal ini ikut ditutup (koneksi SFTP memakai
	// ssh.Client yang sama, jadi mati bersama terminal). Dilakukan
	// dulu, sebelum hwnd terminal dinolkan.
	closeSFTPForTerm(t)
	// Hentikan timer auto-scroll drag sebelum jendela dihancurkan
	// (setelah hwnd dinolkan, stopSelAutoScroll tidak bisa KillTimer).
	if t.selAutoScroll {
		win.KillTimer(t.hwnd, timerSelAutoScroll)
		t.selAutoScroll = false
	}
	if t.selDragging {
		t.selDragging = false
		win.ReleaseCapture()
	}
	if t.mouseTrackBtn >= 0 {
		t.mouseTrackBtn = -1
		win.ReleaseCapture()
	}
	// Simpan hwnd dulu, lalu nolkan supaya destroy kedua untuk term
	// yang sama (mis. Cancel lalu hasil connect terlambat) tidak
	// memanggil DestroyWindow dua kali pada handle yang sama
	// (handle bisa dipakai ulang Windows untuk jendela baru).
	hwnd := t.hwnd
	t.hwnd = 0
	delete(termWins, hwnd)
	removeTermOrder(hwnd)
	if pendingTerm == t {
		pendingTerm = nil
	}
	t.cfg = Session{}
	win.DestroyWindow(hwnd)
	updateSFTPButton()
}


type confirmRequest struct {
	host, fp, warning string
	result            chan bool
}

type connectResult struct {
	gen  int
	conn *Conn
	err  error
	term *termWin
}

// Pesan WM_APP tidak boleh membawa pointer Go mentah lewat uintptr
// (vet mengeluh & GC bisa bermasalah). Pakai ID + map yang dikunci.
var (
	msgMu        sync.Mutex
	msgNextID    uintptr = 1
	confirmByID  = map[uintptr]*confirmRequest{}
	connectByID  = map[uintptr]*connectResult{}
	disconnectByID = map[uintptr]disconnectInfo{}
)

func storeConfirm(req *confirmRequest) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	confirmByID[id] = req
	return id
}

func takeConfirm(id uintptr) *confirmRequest {
	msgMu.Lock()
	defer msgMu.Unlock()
	req := confirmByID[id]
	delete(confirmByID, id)
	return req
}

func storeConnect(res *connectResult) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	connectByID[id] = res
	return id
}

func takeConnect(id uintptr) *connectResult {
	msgMu.Lock()
	defer msgMu.Unlock()
	res := connectByID[id]
	delete(connectByID, id)
	return res
}

func storeDisconnect(err error) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	disconnectByID[id] = disconnectReason(err)
	return id
}

func takeDisconnect(id uintptr) (disconnectInfo, bool) {
	msgMu.Lock()
	defer msgMu.Unlock()
	info, ok := disconnectByID[id]
	delete(disconnectByID, id)
	return info, ok
}

// termWho mengembalikan label server untuk keterangan disconnect:
// "user@host" untuk SSH, "host" saja untuk Telnet.
func termWho(t *termWin) string {
	if t == nil {
		return ""
	}
	if t.cfg.Protocol == "telnet" {
		return t.cfg.Host
	}
	if t.cfg.Username != "" {
		return t.cfg.Username + "@" + t.cfg.Host
	}
	return t.cfg.Host
}

func utf16(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// initAppIcon menentukan icon aplikasi/jendela (2026-10-29, atas
// permintaan user "icon aplikasi/jendela terminal").
// Urutan:
//  1. Override runtime: icon.ico / ssh-terminal.ico di samping .exe,
//     atau %APPDATA%\SSHTerminal\icon.ico. File HARUS .ico (bukan
//     PNG/JPG langsung) karena LoadImage IMAGE_ICON dari file hanya
//     terima .ico. Override ini mengubah icon jendela, taskbar, dan
//     Alt+Tab — BUKAN icon file .exe di Explorer (itu dari resource
//     embedded, hanya berubah kalau .exe di-rebuild dengan .ico baru).
//  2. Icon embedded dari rsrc: manifest ID 1, group icon ID 2, lalu
//     icon per-ukuran ID 3+. Coba ID 2 dulu, lalu ID 1 (jaga-jaga
//     urutan resource berubah di rsrc versi lain).
//  3. Gagal semua: biarkan 0, Windows pakai icon default.
// Dipanggil di main() SEBELUM RegisterClassEx, supaya HIcon/HIconSm
// kelas jendela sudah benar sejak awal.
func initAppIcon() {
	exePath, err := os.Executable()
	candidates := []string{}
	if err == nil {
		dir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(dir, "icon.ico"),
			filepath.Join(dir, "ssh-terminal.ico"))
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		candidates = append(candidates,
			filepath.Join(appdata, "SSHTerminal", "icon.ico"))
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		big := win.LoadImage(0, utf16(path), win.IMAGE_ICON, 32, 32, win.LR_LOADFROMFILE)
		if big == 0 {
			continue
		}
		appIcon = win.HICON(big)
		small := win.LoadImage(0, utf16(path), win.IMAGE_ICON, 16, 16, win.LR_LOADFROMFILE)
		if small != 0 {
			appIconSm = win.HICON(small)
		} else {
			appIconSm = appIcon
		}
		logDebug("app icon override loaded from " + path)
		return
	}
	// hInst global masih 0 di kode ini (sengaja tidak diubah supaya
	// perilaku CreateWindow lama tidak berubah); untuk LoadIcon pakai
	// module handle exe yang benar.
	mod := win.GetModuleHandle(nil)
	for _, id := range []uintptr{2, 1} {
		h := win.LoadIcon(mod, win.MAKEINTRESOURCE(id))
		if h != 0 {
			appIcon = h
			appIconSm = h
			logDebug("app icon embedded loaded")
			return
		}
	}
}

// setWindowIcon memaksa icon jendela tertentu (pelengkap HIcon kelas,
// supaya icon langsung benar meski kelas dibuat sebelum icon siap).
func setWindowIcon(hwnd win.HWND) {
	if appIcon != 0 {
		win.SendMessage(hwnd, win.WM_SETICON, uintptr(1), uintptr(appIcon)) // ICON_BIG
	}
	if appIconSm != 0 {
		win.SendMessage(hwnd, win.WM_SETICON, uintptr(0), uintptr(appIconSm)) // ICON_SMALL
	}
}

func setText(hwnd win.HWND, s string) { setWindowText(hwnd, s) }

// appendStatus appends one line to the status box below the Connect
// button, instead of replacing it. The status box is a log: each event
// (Connecting..., Connected to..., Disconnected from..., Failed...,
// Saved..., etc.) adds a line, nothing is deleted, and the view is
// scrolled to the latest line so the newest event is always visible.
// Called only from the UI thread (all status updates go through
// WndProc / doConnect on that thread).
func appendStatus(line string) {
	if ctlStatus == 0 {
		return
	}
	old := getText(ctlStatus)
	next := line
	if old != "" {
		next = old + "\r\n" + line
	}
	setText(ctlStatus, next)
	// Scroll to latest: caret to end of text, then scroll caret into
	// view. WM_GETTEXTLENGTH gives the length in characters, which is
	// what EM_SETSEL expects (not byte length).
	n := win.SendMessage(ctlStatus, win.WM_GETTEXTLENGTH, 0, 0)
	win.SendMessage(ctlStatus, win.EM_SETSEL, n, n)
	win.SendMessage(ctlStatus, win.EM_SCROLLCARET, 0, 0)
}
func getText(hwnd win.HWND) string    { return getWindowText(hwnd) }

func msgBox(hwnd win.HWND, text, title string, typ uint32) int32 {
	return win.MessageBox(hwnd, utf16(text), utf16(title), typ)
}

// ---------- sessions persistence ----------

func loadSessions() {
	sessions = nil
	b, err := os.ReadFile(sessionsPath())
	if err != nil {
		return
	}
	// Sesi lama (sebelum ada GreenPrompt) tidak punya field itu di JSON.
	// Tanpa penanganan khusus, field bool itu jadi false dan prompt
	// hijau mati untuk sesi lama — padahal default yang diminta user
	// adalah hijau. Jadi: kalau key GreenPrompt tidak ada di JSON,
	// anggap true.
	var raw []map[string]json.RawMessage
	if json.Unmarshal(b, &raw) == nil {
		for _, m := range raw {
			if _, ok := m["GreenPrompt"]; !ok {
				m["GreenPrompt"] = json.RawMessage("true")
			}
		}
		if nb, err := json.Marshal(raw); err == nil {
			b = nb
		}
	}
	_ = json.Unmarshal(b, &sessions)
	// Bersihkan rahasia dari file lama (sebelum perbaikan ini):
	// kalau sessions.json lama berisi password/passphrase plaintext,
	// jangan biarkan masuk ke memori, dan simpan berikutnya akan
	// menimpa file itu dengan versi tanpa rahasia.
	for i := range sessions {
		sessions[i].Password = ""
		sessions[i].KeyPass = ""
		sessions[i].KeyData = nil
	}
}

func saveSessions() {
	_ = os.MkdirAll(configDir(), 0700)
	// Pertahanan kedua (selain json:"-"): salin sesi tanpa rahasia
	// sebelum marshal, supaya perubahan struct di masa depan tidak
	// bisa membocorkan password/passphrase lagi lewat file ini.
	clean := make([]Session, len(sessions))
	for i, s := range sessions {
		s.Password = ""
		s.KeyPass = ""
		s.KeyData = nil
		clean[i] = s
	}
	b, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(sessionsPath(), b, 0600)
}

func refreshSessionList() {
	win.SendMessage(ctlList, win.LB_RESETCONTENT, 0, 0)
	for _, s := range sessions {
		win.SendMessage(ctlList, win.LB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16(s.Name))))
	}
}

func currentSessionFromForm() Session {
	proto := "ssh"
	if win.SendMessage(ctlProto, win.CB_GETCURSEL, 0, 0) == 1 {
		proto = "telnet"
	}
	port, _ := strconv.Atoi(strings.TrimSpace(getText(ctlPort)))
	if port <= 0 {
		if proto == "telnet" {
			port = 23
		} else {
			port = 22
		}
	}
	auth := "password"
	sel := win.SendMessage(ctlAuth, win.CB_GETCURSEL, 0, 0)
	if sel == 1 {
		auth = "key"
	}
	greenPrompt := win.SendMessage(ctlGreenPrompt, win.BM_GETCHECK, 0, 0) == win.BST_CHECKED
	return Session{
		Name:        strings.TrimSpace(getText(ctlSessionName)),
		Host:        strings.TrimSpace(getText(ctlHost)),
		Port:        port,
		Protocol:    proto,
		GreenPrompt: greenPrompt,
		Username: strings.TrimSpace(getText(ctlUser)),
		Auth:     auth,
		Password: getText(ctlPass),
		KeyPath:  strings.TrimSpace(getText(ctlKeyPath)),
		KeyPass:  getText(ctlKeyPass),
	}
}

func moveSessionSelection(delta int) {
	count := int(win.SendMessage(ctlList, win.LB_GETCOUNT, 0, 0))
	if count <= 0 {
		return
	}
	cur := int(win.SendMessage(ctlList, win.LB_GETCURSEL, 0, 0))
	if cur < 0 {
		cur = 0
	} else {
		cur += delta
	}
	if cur < 0 {
		cur = 0
	}
	if cur >= count {
		cur = count - 1
	}
	win.SendMessage(ctlList, win.LB_SETCURSEL, uintptr(cur), 0)
	if cur >= 0 && cur < len(sessions) {
		fillForm(sessions[cur])
	}
	// BUG #1: setelah pilih sesi pakai Up/Down, fokus harus pindah ke
	// Connect dan Enter berikutnya harus Connect — bukan mengklik
	// tombol yang tadinya fokus (mis. New, yang kalau diklik Enter
	// akan membuat sesi kosong baru lagi). Flag ini juga dibaca
	// handler Enter di message loop, dan dibersihkan saat Tab ditekan.
	sessionSelectedViaKeys = true
	win.SetFocus(win.GetDlgItem(hwndSession, idConnect))
}

func fillForm(s Session) {
	setText(ctlHost, s.Host)
	setText(ctlPort, strconv.Itoa(s.Port))
	setText(ctlUser, s.Username)
	if s.Auth == "key" {
		win.SendMessage(ctlAuth, win.CB_SETCURSEL, 1, 0)
	} else {
		win.SendMessage(ctlAuth, win.CB_SETCURSEL, 0, 0)
	}
	setText(ctlPass, "")
	setText(ctlKeyPath, s.KeyPath)
	setText(ctlKeyPass, "")
	if s.Protocol == "telnet" {
		win.SendMessage(ctlProto, win.CB_SETCURSEL, 1, 0)
	} else {
		win.SendMessage(ctlProto, win.CB_SETCURSEL, 0, 0)
	}
	if s.GreenPrompt {
		win.SendMessage(ctlGreenPrompt, win.BM_SETCHECK, win.BST_CHECKED, 0)
	} else {
		win.SendMessage(ctlGreenPrompt, win.BM_SETCHECK, win.BST_UNCHECKED, 0)
	}
	setText(ctlSessionName, s.Name)
	updateAuthVisibility(hwndSession)
	keyFileData = nil
	keyFileLoaded = ""
}

func updateAuthVisibility(hwnd win.HWND) {
	show := func(h win.HWND, v bool) {
		if v {
			win.ShowWindow(h, win.SW_SHOW)
		} else {
			win.ShowWindow(h, win.SW_HIDE)
		}
	}
	// Telnet tidak punya Auth/password/key di protokolnya: login diketik
	// langsung di prompt server di dalam terminal. Sembunyikan seluruh
	// baris Auth/Password/Key/Passphrase supaya tidak membingungkan.
	if win.SendMessage(ctlProto, win.CB_GETCURSEL, 0, 0) == 1 {
		// Checkbox prompt hijau TETAP tampil untuk Telnet: recolor
		// klien tidak mengirim perintah ke server, jadi aman untuk
		// perangkat Telnet (tidak seperti injeksi PS1 dulu).
		show(win.GetDlgItem(hwnd, idAuthLabel), false)
		show(ctlAuth, false)
		show(win.GetDlgItem(hwnd, idPassLabel), false)
		show(ctlPass, false)
		show(win.GetDlgItem(hwnd, idKeyLabel), false)
		show(ctlKeyPath, false)
		show(win.GetDlgItem(hwnd, idBrowse), false)
		show(win.GetDlgItem(hwnd, idKeyPassLabel), false)
		show(ctlKeyPass, false)
		return
	}
	isKey := win.SendMessage(ctlAuth, win.CB_GETCURSEL, 0, 0) == 1
	show(win.GetDlgItem(hwnd, idGreenPrompt), true)
	show(win.GetDlgItem(hwnd, idAuthLabel), true)
	show(ctlAuth, true)
	show(win.GetDlgItem(hwnd, idPassLabel), !isKey)
	show(ctlPass, !isKey)
	show(win.GetDlgItem(hwnd, idKeyLabel), isKey)
	show(ctlKeyPath, isKey)
	show(win.GetDlgItem(hwnd, idBrowse), isKey)
	show(win.GetDlgItem(hwnd, idKeyPassLabel), isKey)
	show(ctlKeyPass, isKey)
}

func setCtrlFont(hwnd win.HWND, font win.HFONT) {
	if hwnd != 0 && font != 0 {
		win.SendMessage(hwnd, win.WM_SETFONT, uintptr(font), 1)
	}
}

func setFontsForSession(hwnd win.HWND) {
	// Set Segoe UI untuk SEMUA kontrol anak (termasuk label STATIC),
	// supaya tidak ada yang tertinggal pakai font dialog klasik.
	cb := syscall.NewCallback(func(child win.HWND, _ uintptr) uintptr {
		win.SendMessage(child, win.WM_SETFONT, uintptr(uiFont), 1)
		return 1
	})
	// Simpan callback agar tidak di-GC selama window hidup.
	fontEnumCallback = cb
	win.EnumChildWindows(hwnd, cb, 0)
	setCtrlFont(win.GetDlgItem(hwnd, idHeaderTitle), headerFont)
	setCtrlFont(win.GetDlgItem(hwnd, idHeaderSub), headerSubFont)
	setCtrlFont(win.GetDlgItem(hwnd, idConnect), uiFontBold)
}

// ---------- terminal helpers ----------

func termColsRows(t *termWin) (int, int) {
	var rc win.RECT
	win.GetClientRect(t.hwnd, &rc)
	w := rc.Right - rc.Left
	if sbW := win.GetSystemMetrics(win.SM_CXVSCROLL); sbW > 0 && w > sbW {
		w -= sbW
	}
	h := rc.Bottom - rc.Top
	if charW <= 0 || charH <= 0 {
		return 80, 24
	}
	cols := int(w / charW)
	rows := int(h / charH)
	if cols < 20 {
		cols = 20
	}
	if rows < 5 {
		rows = 5
	}
	return cols, rows
}
func applyTermSize(t *termWin) {
	if t == nil {
		return
	}
	// Pertahanan kedua untuk bug minimize 2026-10-29: kalau ukuran
	// klien 0 (jendela diminimize / dalam transisi), jangan resize
	// sama sekali — termColsRows akan mengembalikan minimum 20x5
	// dan memotong buffer. Jalur utama pencegahannya ada di WM_SIZE
	// (lewati SIZE_MINIMIZED), ini hanya jaring pengaman kalau
	// applyTermSize terpanggil dari jalur lain saat klien 0.
	var rc win.RECT
	win.GetClientRect(t.hwnd, &rc)
	if rc.Right-rc.Left <= 0 || rc.Bottom-rc.Top <= 0 {
		return
	}
	cols, rows := termColsRows(t)
	if t.term != nil && (cols != t.term.Cols || rows != t.term.Rows) {
		t.term.Resize(cols, rows)
		connMu.Lock()
		c := t.conn
		connMu.Unlock()
		if c != nil {
			c.SendWindowChange(cols, rows)
		}
	}
}
// postOutput/postClosed dulu global (satu antrean, satu hwndTerm) —
// kini diganti closure per-terminal di startConnection, supaya output
// terminal A tidak pernah masuk ke terminal B.

func copySelectionToClipboard(t *termWin) {
	if t == nil || !t.selActive || t.term == nil {
		return
	}
	// Seleksi disimpan absolut (bukan view-relative), jadi copy tetap
	// benar walau tampilan sempat di-scroll sesudah drag selesai —
	// mis. drag panjang melewati scrollback, lalu scroll kembali ke
	// live sebelum klik kanan / Ctrl+Shift+C.
	y1, x1, y2, x2 := orderedSelection(t.selStartAbsY, t.selStartX, t.selEndAbsY, t.selEndX)
	var sb strings.Builder
	for y := y1; y <= y2; y++ {
		line := t.term.LineTextAbs(y)
		if y == y1 && y == y2 {
			if x1 < len(line) {
				end := x2
				if end > len(line) {
					end = len(line)
				}
				if x1 < end {
					sb.WriteString(line[x1:end])
				}
			}
		} else if y == y1 {
			if x1 < len(line) {
				sb.WriteString(line[x1:])
			}
		} else if y == y2 {
			end := x2
			if end > len(line) {
				end = len(line)
			}
			sb.WriteString(line[:end])
		} else {
			sb.WriteString(line)
		}
		if y < y2 {
			sb.WriteString("\r\n")
		}
	}
	text := sb.String()
	if text == "" {
		return
	}
	if !win.OpenClipboard(t.hwnd) {
		return
	}
	win.EmptyClipboard()
	utf, _ := windows.UTF16FromString(text)
	size := uintptr(len(utf)) * 2
	hMem := win.GlobalAlloc(win.GMEM_MOVEABLE, size)
	if hMem != 0 {
		p := win.GlobalLock(hMem)
		dst := (*[1 << 30]byte)(p)
		for i, u := range utf {
			dst[i*2] = byte(u)
			dst[i*2+1] = byte(u >> 8)
		}
		win.GlobalUnlock(hMem)
		win.SetClipboardData(win.CF_UNICODETEXT, win.HANDLE(hMem))
	}
	win.CloseClipboard()
}
func getClipboardText(owner win.HWND) string {
	if !win.OpenClipboard(owner) {
		return ""
	}
	h := win.GetClipboardData(win.CF_UNICODETEXT)
	if h == 0 {
		win.CloseClipboard()
		return ""
	}
	p := win.GlobalLock(win.HGLOBAL(h))
	if p == nil {
		win.CloseClipboard()
		return ""
	}
	u16 := (*[1 << 28]uint16)(p)
	n := 0
	for n < len(u16) && u16[n] != 0 {
		n++
	}
	text := windows.UTF16ToString(u16[:n])
	win.GlobalUnlock(win.HGLOBAL(h))
	win.CloseClipboard()
	return text
}
func pasteFromClipboard(t *termWin) {
	if t == nil || t.disconnected {
		// Terminal putus: paste ke server yang sudah mati tidak ada
		// gunanya, dan reset scrollOffset akan mengganggu user yang
		// sedang membaca output terakhir di scrollback.
		return
	}
	text := getClipboardText(t.hwnd)
	if text == "" {
		return
	}
	t.scrollOffset = 0
	updateTermScrollbar(t)
	text = strings.ReplaceAll(text, "\r\n", "\r")
	text = strings.ReplaceAll(text, "\n", "\r")
	// Bracketed paste (mode 2004, seperti PuTTY): kalau aplikasi
	// mengaktifkannya (vim/nano modern), bungkus paste supaya
	// aplikasi tahu ini paste, bukan ketikan — jadi tidak
	// auto-indent berantakan. Kalau mode mati, kirim mentah.
	if t.term != nil && t.term.BracketedPaste {
		text = bracketedPasteWrap(text, true)
	}
	connMu.Lock()
	c := t.conn
	connMu.Unlock()
	if c != nil {
		_, _ = c.Stdin.Write([]byte(text))
	}
}
func clampScrollOffset(t *termWin) {
	if t == nil || t.term == nil {
		if t != nil {
			t.scrollOffset = 0
		}
		return
	}
	if t.term.AltActive() {
		t.scrollOffset = 0
		return
	}
	if t.scrollOffset < 0 {
		t.scrollOffset = 0
	}
	if max := t.term.ScrollbackLen(); t.scrollOffset > max {
		t.scrollOffset = max
	}
}
func updateTermScrollbar(t *termWin) {
	if t == nil || t.hwnd == 0 || t.term == nil {
		return
	}
	sb := t.term.ScrollbackLen()
	if t.term.AltActive() {
		sb = 0
	}
	rows := t.term.Rows
	if rows < 1 {
		rows = 1
	}
	pos := sb - t.scrollOffset
	if pos < 0 {
		pos = 0
	}
	if pos > sb {
		pos = sb
	}
	si := win.SCROLLINFO{
		CbSize: uint32(unsafe.Sizeof(win.SCROLLINFO{})),
		FMask:  win.SIF_RANGE | win.SIF_PAGE | win.SIF_POS | win.SIF_DISABLENOSCROLL,
		NMin:   0,
		NMax:   int32(sb + rows - 1),
		NPage:  uint32(rows),
		NPos:   int32(pos),
	}
	win.SetScrollInfo(t.hwnd, win.SB_VERT, &si, true)
}
func setScrollOffset(t *termWin, v int) {
	if t == nil {
		return
	}
	t.scrollOffset = v
	clampScrollOffset(t)
	updateTermScrollbar(t)
	win.InvalidateRect(t.hwnd, nil, false)
}
func writeToSSH(t *termWin, b []byte) {
	if t == nil {
		return
	}
	connMu.Lock()
	c := t.conn
	connMu.Unlock()
	if c != nil {
		_, _ = c.Stdin.Write(b)
	}
}
func sendToSSH(t *termWin, b []byte) {
	if t == nil || t.disconnected {
		// Terminal putus: jangan kirim ke server yang sudah mati, dan
		// jangan reset scrollOffset — user mungkin sedang membaca
		// output terakhir di scrollback. Scroll (Shift+PgUp/dll,
		// wheel) dan seleksi/copy tetap jalan, karena jalurnya tidak
		// lewat sini.
		return
	}
	t.scrollOffset = 0
	updateTermScrollbar(t)
	writeToSSH(t, b)
}
// mouseTrackingActive: true kalau aplikasi mengaktifkan mouse
// reporting DAN tampilan sedang di live (scrollOffset 0). Kalau user
// sedang membaca scrollback, klik/wheel harus seleksi/scroll lokal,
// bukan dikirim ke aplikasi dengan koordinat yang salah.
func mouseTrackingActive(t *termWin) bool {
	return t != nil && t.term != nil && t.term.MouseMode != 0 && t.scrollOffset == 0 && !t.disconnected && !t.pwActive
}

// sendMouse mengirim satu event mouse ke aplikasi (PuTTY 1000/1002/
// 1003, SGR 1006 kalau aktif, kalau tidak X10). x, y adalah sel
// 0-based (di-clamp oleh cellFromMouse). Kembali false kalau mode
// mouse tidak aktif.
func sendMouse(t *termWin, base, cx, cy int, release, motion bool) bool {
	if !mouseTrackingActive(t) {
		return false
	}
	shift := win.GetKeyState(win.VK_SHIFT) < 0
	ctrl := win.GetKeyState(win.VK_CONTROL) < 0
	alt := win.GetKeyState(win.VK_MENU) < 0
	btn := mouseButton(base, shift, ctrl, alt, motion)
	sendToSSH(t, mouseSeq(btn, cx+1, cy+1, t.term.MouseSGR, release))
	return true
}

func createControl(className, text string, style uint32, x, y, w, h int32, parent win.HWND, id int) win.HWND {
	st := win.WS_CHILD | win.WS_VISIBLE | style
	if className != "STATIC" {
		st |= win.WS_TABSTOP
	}
	return win.CreateWindowEx(0, utf16(className), utf16(text),
		st, x, y, w, h, parent, win.HMENU(uintptr(id)), hInst, nil)
}

func sessionWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case win.WM_CTLCOLORSTATIC:
		// Header banner: dark blue background, white text.
		// Other labels: light session background, dark text.
		hdc := win.HDC(wParam)
		ctl := win.HWND(lParam)
		if ctl == win.GetDlgItem(hwnd, idHeaderBanner) || ctl == win.GetDlgItem(hwnd, idHeaderTitle) || ctl == win.GetDlgItem(hwnd, idHeaderSub) {
			win.SetBkColor(hdc, rgb(0x1F, 0x4E, 0x79))
			win.SetTextColor(hdc, rgb(0xFF, 0xFF, 0xFF))
			win.SetBkMode(hdc, win.OPAQUE)
			return uintptr(accentBrush)
		}
		win.SetBkColor(hdc, rgb(0xF2, 0xF4, 0xF7))
		win.SetTextColor(hdc, rgb(0x1F, 0x29, 0x37))
		win.SetBkMode(hdc, win.TRANSPARENT)
		return uintptr(sessionBgBrush)
	case win.WM_CREATE:
		// Header banner (dark blue) + title/subtitle in Segoe UI.
		createControl("STATIC", "", 0, 0, 0, 580, 64, hwnd, idHeaderBanner)
		createControl("STATIC", "KERM Terminal", 0, 16, 8, 300, 30, hwnd, idHeaderTitle)
		createControl("STATIC", "Lightning modern terminal", 0, 16, 36, 400, 20, hwnd, idHeaderSub)
		// URUTAN PEMBUATAN = URUTAN TAB. Windows menab sesuai z-order
		// (urutan pembuatan), jadi kontrol dibuat sesuai urutan Tab yang
		// diminta user: Nama sesi -> Host -> Port -> Username -> Protokol ->
		// Auth -> Password -> File key -> Browse -> Passphrase -> Connect ->
		// Batal -> Simpan -> Hapus -> New -> Daftar sesi -> (kembali ke
		// Nama sesi).
		// Posisi visual (y) ditulis eksplisit per baris, jadi urutan
		// pembuatan tidak harus sama dengan urutan visual dari atas ke bawah.
		// Baris Nama sesi (y=202)
		createControl("STATIC", "Session name:", 0, 16, 202, 110, 22, hwnd, 0)
		ctlSessionName = createControl("EDIT", "Localhost", win.WS_BORDER|win.ES_AUTOHSCROLL, 140, 202, 200, 22, hwnd, idSessionName)
		// Baris Host (y=234)
		createControl("STATIC", "Host:", 0, 16, 234, 110, 22, hwnd, 0)
		ctlHost = createControl("EDIT", "127.0.0.1", win.WS_BORDER|win.ES_AUTOHSCROLL, 140, 234, 260, 22, hwnd, idHost)
		createControl("STATIC", "Port:", 0, 410, 234, 30, 22, hwnd, 0)
		ctlPort = createControl("EDIT", "22", win.WS_BORDER|win.ES_AUTOHSCROLL, 442, 234, 60, 22, hwnd, idPort)
		// Baris Username (y=266)
		createControl("STATIC", "Username:", 0, 16, 266, 110, 22, hwnd, 0)
		ctlUser = createControl("EDIT", "root", win.WS_BORDER|win.ES_AUTOHSCROLL, 140, 266, 200, 22, hwnd, idUser)
		// Baris Protokol (y=298) — SSH (default) atau Telnet.
		createControl("STATIC", "Protocol:", 0, 16, 298, 110, 22, hwnd, 0)
		ctlProto = createControl("COMBOBOX", "", win.CBS_DROPDOWNLIST|win.WS_VSCROLL, 140, 298, 200, 100, hwnd, idProtoCombo)
		win.SendMessage(ctlProto, win.CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16("SSH"))))
		win.SendMessage(ctlProto, win.CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16("Telnet"))))
		win.SendMessage(ctlProto, win.CB_SETCURSEL, 0, 0)
		// Baris Auth (y=330)
		createControl("STATIC", "Auth:", 0, 16, 330, 110, 22, hwnd, idAuthLabel)
		ctlAuth = createControl("COMBOBOX", "", win.CBS_DROPDOWNLIST|win.WS_VSCROLL, 140, 330, 200, 100, hwnd, idAuthCombo)
		win.SendMessage(ctlAuth, win.CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16("Password"))))
		win.SendMessage(ctlAuth, win.CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16("Key (OpenSSH / .ppk)"))))
		win.SendMessage(ctlAuth, win.CB_SETCURSEL, 0, 0)
		// Baris Password / File key (y=330)
		createControl("STATIC", "Password:", 0, 16, 362, 110, 22, hwnd, idPassLabel)
		ctlPass = createControl("EDIT", "", win.WS_BORDER|win.ES_AUTOHSCROLL|win.ES_PASSWORD, 140, 362, 200, 22, hwnd, idPass)
		createControl("STATIC", "Key file:", 0, 16, 362, 110, 22, hwnd, idKeyLabel)
		ctlKeyPath = createControl("EDIT", "", win.WS_BORDER|win.ES_AUTOHSCROLL, 140, 362, 260, 22, hwnd, idKeyPath)
		createControl("BUTTON", "Browse...", 0, 410, 362, 64, 24, hwnd, idBrowse)
		// Baris Passphrase (y=362)
		createControl("STATIC", "Key passphrase:", 0, 16, 394, 110, 22, hwnd, idKeyPassLabel)
		ctlKeyPass = createControl("EDIT", "", win.WS_BORDER|win.ES_AUTOHSCROLL|win.ES_PASSWORD, 140, 394, 200, 22, hwnd, idKeyPass)
		// Baris tombol Connect / Batal (y=398)
		createControl("BUTTON", "Connect", win.BS_DEFPUSHBUTTON, 140, 430, 100, 28, hwnd, idConnect)
		ctlCancel = createControl("BUTTON", "Cancel", 0, 250, 430, 80, 28, hwnd, idCancelConnect)
		win.EnableWindow(ctlCancel, false)
		// Checkbox prompt hijau (bash, SSH saja; disembunyikan saat Telnet).
		// Default dicentang, sesuai permintaan user: prompt harus hijau,
		// bukan hanya banner di awal.
		ctlGreenPrompt = createControl("BUTTON", "Green prompt", win.BS_AUTOCHECKBOX, 340, 430, 170, 24, hwnd, idGreenPrompt)
		win.SendMessage(ctlGreenPrompt, win.BM_SETCHECK, win.BST_CHECKED, 0)
		// Baris Simpan / Hapus / New (y=202, di kanan Nama sesi)
		createControl("BUTTON", "Save", 0, 350, 202, 50, 24, hwnd, idSaveSession)
		createControl("BUTTON", "Delete", 0, 406, 202, 50, 24, hwnd, idDeleteSession)
		createControl("BUTTON", "New", 0, 462, 202, 50, 24, hwnd, idNewSession)
		// Daftar sesi (y=80 label, y=100 list) — terakhir di urutan Tab,
		// supaya Tab dari New langsung ke daftar, lalu kembali ke Nama sesi.
		createControl("STATIC", "Saved sessions:", 0, 16, 80, 140, 22, hwnd, 0)
		ctlList = createControl("LISTBOX", "", win.WS_VSCROLL|win.LBS_NOTIFY, 16, 100, 516, 90, hwnd, idSessionList)
		// Status (y=468) — tidak ikut Tab (readonly, hapus WS_TABSTOP).
		// 2026-10-29: panel diperbesar sedikit (560x640 -> 580x700)
		// supaya log terbaca rapi — status box diperpanjang dari
		// 52 ke 110 px (dari ~2-3 baris terlihat jadi ~5 baris),
		// dengan margin kanan/bawah yang sama seperti sebelumnya.
		ctlStatus = createControl("EDIT", "",
			win.WS_BORDER|win.ES_MULTILINE|win.ES_READONLY|win.WS_VSCROLL, 16, 468, 516, 110, hwnd, idStatus)
		{
			st := win.GetWindowLong(ctlStatus, win.GWL_STYLE)
			win.SetWindowLong(ctlStatus, win.GWL_STYLE, st &^ int32(win.WS_TABSTOP))
		}
		// Tombol SFTP (2026-10-30): di bawah status log, supaya
		// tidak menggeser layout panel yang sudah ada. Membuka
		// jendela SFTP untuk sesi yang sudah terhubung; kalau belum
		// terhubung, status log memberi tahu untuk Connect dulu.
		// Jalur utama tetap Ctrl+Shift+S di terminal ("SFTP here").
		createControl("BUTTON", "SFTP", 0, 16, 590, 80, 28, hwnd, idSFTPButton)
		createControl("STATIC", "Open SFTP for the connected session (or Ctrl+Shift+S in the terminal).", 0, 106, 594, 420, 22, hwnd, 0)
		loadSessions()
		refreshSessionList()
		updateAuthVisibility(hwnd)
		setFontsForSession(hwnd)
		updateSFTPButton()
		logDebug("session WM_CREATE done")
		return 0
	case win.WM_COMMAND:
		id := int32(wParam & 0xFFFF)
		code := int32(wParam >> 16)
		switch id {
		case idConnect:
			if code == win.BN_CLICKED {
				doConnect()
			}
		case idSFTPButton:
			if code == win.BN_CLICKED {
				openSFTPFromPanel()
			}
		case idCancelConnect:
			if code == win.BN_CLICKED && connectInProg {
				// Batalkan secara UI: hasil koneksi yang terlambat
				// akan dibuang karena generasinya sudah tidak cocok,
				// dan jendela terminal yang sedang dihubungkan
				// dihancurkan (bukan dibiarkan tersembunyi).
				connectGen++
				connectInProg = false
				win.EnableWindow(win.GetDlgItem(hwnd, idConnect), true)
				win.EnableWindow(ctlCancel, false)
				if pendingTerm != nil {
					destroyTermWindow(pendingTerm)
				}
				appendStatus("Cancelled.")
			}
		case idSaveSession:
			if code == win.BN_CLICKED {
				s := currentSessionFromForm()
				if s.Name == "" {
					appendStatus("Session name is empty.")
					break
				}
				found := false
				for i := range sessions {
					if sessions[i].Name == s.Name {
						sessions[i] = s
						found = true
					}
				}
				if !found {
					sessions = append(sessions, s)
				}
				saveSessions()
				refreshSessionList()
				appendStatus("Session '"+s.Name+"' saved (password not saved).")
			}
		case idDeleteSession:
			if code == win.BN_CLICKED {
				idx := int(win.SendMessage(ctlList, win.LB_GETCURSEL, 0, 0))
				if idx >= 0 && idx < len(sessions) {
					name := sessions[idx].Name
					sessions = append(sessions[:idx], sessions[idx+1:]...)
					saveSessions()
					refreshSessionList()
					appendStatus("Session '"+name+"' deleted.")
				}
			}
		case idNewSession:
			if code == win.BN_CLICKED {
				// Reset form ke konfigurasi default, siap diisi ulang.
				// Sesi tersimpan TIDAK dihapus, hanya form yang dikosongkan.
				setText(ctlSessionName, "")
				setText(ctlHost, "127.0.0.1")
				setText(ctlPort, "22")
				setText(ctlUser, "root")
				win.SendMessage(ctlProto, win.CB_SETCURSEL, 0, 0)
				win.SendMessage(ctlGreenPrompt, win.BM_SETCHECK, win.BST_CHECKED, 0)
				win.SendMessage(ctlAuth, win.CB_SETCURSEL, 0, 0)
				setText(ctlPass, "")
				setText(ctlKeyPath, "")
				setText(ctlKeyPass, "")
				win.SendMessage(ctlList, win.LB_SETCURSEL, ^uintptr(0), 0) // -1 = batal pilih
				keyFileData = nil
				keyFileLoaded = ""
				updateAuthVisibility(hwnd)
				appendStatus("New session — fill in Host/Port/Username, then Connect.")
				win.SetFocus(ctlSessionName)
			}
		case idBrowse:
			if code == win.BN_CLICKED {
				if p := browseKeyFile(hwnd); p != "" {
					setText(ctlKeyPath, p)
					keyFileData = nil
					keyFileLoaded = ""
				}
			}
		case idAuthCombo:
			if code == win.CBN_SELCHANGE {
				updateAuthVisibility(hwnd)
			}
		case idProtoCombo:
			if code == win.CBN_SELCHANGE {
				// Ganti port default hanya kalau port masih default protokol
				// lama (jangan timpa port kustom seperti 43727 / 26).
				p := strings.TrimSpace(getText(ctlPort))
				if win.SendMessage(ctlProto, win.CB_GETCURSEL, 0, 0) == 1 {
					if p == "22" {
						setText(ctlPort, "23")
					}
					appendStatus("WARNING: Telnet is NOT encrypted — everything you type (including passwords) is sent in plain text. Use only on local/trusted networks, never over the internet. Login is typed directly in the terminal.")
				} else {
					if p == "23" {
						setText(ctlPort, "22")
					}
				}
				updateAuthVisibility(hwnd)
			}
		case idSessionList:
			if code == win.LBN_DBLCLK {
				// Klik 2x pada sesi = langsung Connect ke sesi itu,
				// bukan hanya mengisi form.
				idx := int(win.SendMessage(ctlList, win.LB_GETCURSEL, 0, 0))
				if idx >= 0 && idx < len(sessions) {
					fillForm(sessions[idx])
					sessionSelectedViaKeys = false
					doConnect()
				}
			} else if code == win.LBN_SELCHANGE {
				idx := int(win.SendMessage(ctlList, win.LB_GETCURSEL, 0, 0))
				if idx >= 0 && idx < len(sessions) {
					fillForm(sessions[idx])
				}
			}
		}
		return 0
	case wmAppConfirm:
		logDebug("wmAppConfirm received, showing MessageBox")
		req := takeConfirm(wParam)
		if req == nil {
			return 0
		}
		text := "Server host key:\n" + req.fp + "\n\nHost: " + req.host
		if req.warning != "" {
			text = req.warning + "\n\n" + text
		} else {
			text += "\n\nContinue and save this host key?"
		}
		req.result <- msgBox(hwnd, text, "Verify Host Key", win.MB_YESNO|win.MB_ICONWARNING) == win.IDYES
		return 0
	case wmAppSFTPPickDone:
		// Hasil pemilih terminal: wParam = HWND terminal yang dipilih,
		// 0 = dibatalkan. Terminal bisa saja putus selagi pemilih
		// terbuka, jadi cek ulang koneksinya sebelum buka SFTP.
		if wParam != 0 {
			if t := termWins[win.HWND(wParam)]; t != nil {
				openSFTPForTerm(t)
			} else {
				appendStatus("SFTP: selected terminal is no longer available.")
			}
		}
		return 0
	case wmAppConnectDone:
		logDebug("wmAppConnectDone received")
		res := takeConnect(wParam)
		if res == nil {
			return 0
		}
		if res.gen != connectGen {
			// Hasil dari koneksi yang sudah dibatalkan: tutup saja.
			if res.conn != nil {
				res.conn.Close()
			}
			if res.term != nil {
				destroyTermWindow(res.term)
			}
			return 0
		}
		connectInProg = false
		win.EnableWindow(win.GetDlgItem(hwnd, idConnect), true)
		win.EnableWindow(ctlCancel, false)
		t := res.term
		if t == nil {
			appendStatus("Failed: terminal not found.")
			msgBox(hwnd, "Could not start the terminal window.", "Connection failed", win.MB_ICONERROR|win.MB_OK)
			return 0
		}
		if res.err != nil {
			appendStatus("Failed: "+res.err.Error())
			// SIMPAN host/port/protokol DULU, SEBELUM destroyTermWindow:
			// destroyTermWindow mengosongkan t.cfg = Session{} di
			// akhirnya, jadi kalau popup dibaca dari t.cfg setelah
			// destroy, host/port-nya hilang (popup jadi
			// "Error connect SSH failed: :0 i/o timeout" — persis
			// laporan user 2026-11-10). Ini bug saya di build
			// sebelumnya, bukan servernya.
			failHost := t.cfg.Host
			failPort := t.cfg.Port
			failProto := t.cfg.Protocol
			// Terminal yang gagal connect dihancurkan, panel sesi
			// ditampilkan lagi untuk retry (panel sudah terlihat
			// selama ini, jadi ShowWindow ini no-op kecuali panel
			// sempat ditutup user).
			destroyTermWindow(t)
			win.ShowWindow(hwnd, win.SW_SHOW)
			win.SetFocus(ctlSessionName)
			// Notifikasi gagal connect (2026-11-10, disederhanakan
			// 2026-11-10 atas permintaan user: popupnya simpel saja).
			// Dulu kegagalan hanya satu baris di status log — gampang
			// terlewat, apalagi kalau user pindah ke aplikasi lain
			// selagi menunggu timeout 10 detik. Sekarang popup
			// MessageBox (bunyi error sistem + panel dibawa ke depan),
			// bentuknya cuma judul singkat + satu baris, contoh:
			//   Judul: Connection Timeout
			//   Isi:   Error connect SSH failed: host:port i/o timeout
			// Tidak ada toast Windows di sini: toast butuh
			// AppUserModelID / shortcut terinstall supaya andal,
			// tidak cocok untuk EXE portable tanpa install.
			// Pengecualian: user membatalkan sendiri (gen tidak
			// cocok, sudah return di atas) dan user menolak host
			// key di dialog (notify=false di helper) — keduanya
			// tidak dapat popup kedua.
			if title, detail, notify := connectFailureReason(res.err); notify {
				proto := "SSH"
				if failProto == "telnet" {
					proto = "Telnet"
				}
				text := "Error connect " + proto + " failed: " + failHost + ":" + strconv.Itoa(failPort) + " " + detail
				msgBox(hwnd, text, title, win.MB_ICONERROR|win.MB_OK)
			}
			return 0
		}
		connMu.Lock()
		t.conn = res.conn
		connMu.Unlock()
		if pendingTerm == t {
			pendingTerm = nil
		}
		// Panel sesi TETAP terlihat (satu-satunya panel pemilihan),
		// terminal baru tampil di depan. Panel tidak pernah
		// disembunyikan saat connect berhasil, supaya user bisa
		// langsung pilih sesi lain untuk terminal berikutnya.
		appendStatus("Connected to "+t.cfg.Username+"@"+t.cfg.Host)
		updateSFTPButton()
		win.ShowWindow(t.hwnd, win.SW_SHOW)
		win.SetFocus(t.hwnd)
		if t.cfg.Protocol == "telnet" {
			setWindowText(t.hwnd, "KERM Terminal - "+t.cfg.Host)
		} else {
			setWindowText(t.hwnd, "KERM Terminal - "+t.cfg.Username+"@"+t.cfg.Host)
		}
		return 0
	case win.WM_CLOSE:
		// Siklus panel/terminal (permintaan user 2026-10-29): kalau
		// terminal masih jalan, tutup panel HANYA menyembunyikan
		// panel (bukan mematikan proses/terminal). Panel dibuka
		// lagi dari terminal dengan Ctrl+Shift+N, atau otomatis
		// saat terminal ditutup (tutup terminal selalu menampilkan
		// panel lagi). Kalau tidak ada terminal aktif, tutup panel
		// = keluar aplikasi.
		if hasActiveTerminal() {
			// Panel disembunyikan saja, terminal tetap jalan.
			// Fokus diserahkan ke Windows (jendela berikutnya di
			// Z-order, biasanya terminal yang terakhir aktif).
			win.ShowWindow(hwnd, win.SW_HIDE)
			return 0
		}
		win.DestroyWindow(hwnd)
		return 0
	case win.WM_DESTROY:
		win.PostQuitMessage(0)
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}

func browseKeyFile(owner win.HWND) string {
	filter, _ := windows.UTF16FromString("Key files (*.ppk;*.pem;*.key)\x00*.ppk;*.pem;*.key\x00All files (*.*)\x00*.*\x00")
	// UTF16FromString adds one trailing null; append another for double-null
	filter = append(filter, 0)
	fileBuf := make([]uint16, 260)
	ofn := win.OPENFILENAME{
		LStructSize: uint32(unsafe.Sizeof(win.OPENFILENAME{})),
		HwndOwner:   owner,
		HInstance:   hInst,
		LpstrFilter: &filter[0],
		LpstrFile:   &fileBuf[0],
		NMaxFile:    260,
		Flags:       0x00080000 | 0x00001000, // OFN_FILEMUSTEXIST | OFN_PATHMUSTEXIST
	}
	if win.GetOpenFileName(&ofn) {
		return windows.UTF16ToString(fileBuf)
	}
	return ""
}

// hasActiveTerminal: true kalau proses ini sedang punya terminal
// (terhubung, sedang menghubungkan, atau sedang di password prompt).
// Dipakai untuk: (a) Connect kedua harus luncurkan proses baru, bukan
// menimpa terminal pertama; (b) tutup panel sesi tidak boleh mematikan
// proses kalau terminal masih aktif.
func hasActiveTerminal() bool {
	return len(termWins) > 0 || connectInProg
}


func doConnect() {
	s := currentSessionFromForm()
	isTelnet := s.Protocol == "telnet"
	if s.Host == "" || (!isTelnet && s.Username == "") {
		if isTelnet {
			appendStatus("Host is required.")
		} else {
			appendStatus("Host and Username are required.")
		}
		return
	}
	// Satu proses, banyak terminal: hanya satu upaya connect yang
	// boleh jalan pada satu waktu (tombol Connect dinonaktifkan
	// selama connect). Terminal yang SUDAH connected tidak
	// menghalangi Connect baru — Connect baru selalu membuat
	// jendela terminal baru di proses yang sama (bukan proses baru).
	if connectInProg {
		return
	}
	if pendingTerm != nil && pendingTerm.pwActive {
		appendStatus("Still waiting for password in the terminal...")
		return
	}
	cfg := Session{Host: s.Host, Port: s.Port, Protocol: s.Protocol, GreenPrompt: s.GreenPrompt, Username: s.Username, Auth: s.Auth, Password: s.Password, KeyPath: s.KeyPath, KeyPass: s.KeyPass}
	if isTelnet {
		startConnection(cfg, true, nil)
		return
	}
	if s.Auth == "key" {
		if s.KeyPath == "" {
			appendStatus("Select a key file first (Browse...).")
			return
		}
		b, err := os.ReadFile(s.KeyPath)
		if err != nil {
			appendStatus("Failed to read key file: "+err.Error())
			return
		}
		cfg.KeyData = b
		if c := PPKComment(string(b)); c != "" {
			cfg.KeyComment = c
		} else {
			cfg.KeyComment = filepath.Base(s.KeyPath)
		}
		startConnection(cfg, true, nil)
		return
	}
	if s.Password != "" {
		startConnection(cfg, true, nil)
		return
	}
	startPasswordPrompt(cfg)
}
func startPasswordPrompt(cfg Session) *termWin {
	t := createTermWindow()
	if t == nil {
		appendStatus("Failed to create terminal window.")
		return nil
	}
	cols, rows := termColsRows(t)
	t.term = NewTerm(cols, rows)
	t.term.PromptRecolor = cfg.GreenPrompt
	t.scrollOffset = 0
	t.selActive = false
	t.selDragging = false
	stopSelAutoScroll(t)
	t.pwCfg = cfg
	t.pwBuf = nil
	t.pwActive = true
	pendingTerm = t
	setWindowText(t.hwnd, "KERM Terminal - "+cfg.Username+"@"+cfg.Host)
	// Panel sesi tetap terlihat (satu-satunya panel pemilihan);
	// terminal password tampil di depan.
	win.ShowWindow(t.hwnd, win.SW_SHOW)
	win.SetFocus(t.hwnd)
	prompt := "\r\nType the password for \x1b[1;32m" + cfg.Username + "@" + cfg.Host + ":\x1b[0m, then Enter. (Characters show as *; Ctrl+C to cancel.)\r\n\x1b[1;32m" + cfg.Username + "@" + cfg.Host + ":\x1b[0m password: "
	t.term.Feed([]byte(prompt))
	win.InvalidateRect(t.hwnd, nil, false)
	appendStatus("Waiting for password in the terminal for "+cfg.Username+"@"+cfg.Host+"...")
	logDebug("password prompt shown host=" + cfg.Host)
	return t
}
func cancelPasswordPrompt(t *termWin) {
	if t == nil {
		return
	}
	t.pwActive = false
	t.pwBuf = nil
	// Batal password = terminal ini tidak jadi connected: hancurkan
	// jendelanya (bukan sekadar hide, supaya tidak menumpuk jendela
	// tersembunyi), lalu kembali ke panel sesi.
	destroyTermWindow(t)
	win.ShowWindow(hwndSession, win.SW_SHOW)
	win.SetFocus(ctlSessionName)
	appendStatus("Cancelled (password prompt).")
	logDebug("password prompt cancelled")
}
func finishPasswordPrompt(t *termWin) {
	if t == nil {
		return
	}
	pw := string(t.pwBuf)
	cfg := t.pwCfg
	t.pwActive = false
	t.pwBuf = nil
	t.term.Feed([]byte("\r\n"))
	win.InvalidateRect(t.hwnd, nil, false)
	cfg.Password = pw
	startConnection(cfg, false, t)
}
func pastePasswordFromClipboard(t *termWin) {
	if t == nil {
		return
	}
	text := getClipboardText(t.hwnd)
	if text == "" {
		return
	}
	for _, r := range text {
		if !t.pwActive {
			return
		}
		if r == '\r' || r == '\n' {
			finishPasswordPrompt(t)
			return
		}
		if r < 0x20 {
			continue
		}
		t.pwBuf = append(t.pwBuf, r)
		t.term.Feed([]byte("*"))
	}
	win.InvalidateRect(t.hwnd, nil, false)
}
func startConnection(cfg Session, resetTerm bool, t *termWin) {
	isTelnet := cfg.Protocol == "telnet"
	if t == nil {
		t = createTermWindow()
		if t == nil {
			appendStatus("Failed to create terminal window.")
			return
		}
	}
	pendingTerm = t
	t.cfg = cfg
	if isTelnet {
		setWindowText(t.hwnd, "KERM Terminal - "+cfg.Host)
	} else {
		setWindowText(t.hwnd, "KERM Terminal - "+cfg.Username+"@"+cfg.Host)
	}
	if resetTerm {
		cols0, rows0 := termColsRows(t)
		t.term = NewTerm(cols0, rows0)
		t.scrollOffset = 0
		t.selActive = false
		t.selDragging = false
		stopSelAutoScroll(t)
		bannerWho := cfg.Host + ":"
		if cfg.Username != "" {
			bannerWho = cfg.Username + "@" + cfg.Host + ":"
		}
		bannerProto := "SSH"
		if isTelnet {
			bannerProto = "Telnet"
		}
		bannerSuffix := bannerProto + " connected"
		if !isTelnet && cfg.Auth == "key" && cfg.KeyComment != "" {
			bannerSuffix = bannerProto + " connected with key '" + cfg.KeyComment + "'"
		}
		t.term.Feed([]byte("\x1b[1;32m" + bannerWho + "\x1b[0m " + bannerSuffix + "\r\n"))
	}
	if t.term != nil {
		t.term.PromptRecolor = cfg.GreenPrompt
	}
	cols, rows := termColsRows(t)
	connectGen++
	gen := connectGen
	connectInProg = true
	win.EnableWindow(win.GetDlgItem(hwndSession, idConnect), false)
	win.EnableWindow(ctlCancel, true)
	if isTelnet {
		appendStatus("Connecting Telnet to "+cfg.Host+"... (UNENCRYPTED — only local/trusted networks; login is typed in the terminal)")
	} else {
		appendStatus("Connecting to "+cfg.Host+"... (window stays responsive, click Cancel to cancel)")
	}
	logDebug("Connect clicked proto=" + cfg.Protocol + " host=" + cfg.Host + " port=" + strconv.Itoa(cfg.Port) + " user=" + cfg.Username + " auth=" + cfg.Auth)
	confirm := func(host, fp, warning string) bool {
		logDebug("hostkey confirm requested host=" + host + " fp=" + fp)
		req := &confirmRequest{host: host, fp: fp, warning: warning, result: make(chan bool, 1)}
		postMessage(hwndSession, wmAppConfirm, storeConfirm(req), 0)
		r := <-req.result
		logDebug("hostkey confirm answered")
		return r
	}
	postOut := func(data []byte) {
		t.outMu.Lock()
		t.outQueue = append(t.outQueue, data)
		t.outMu.Unlock()
		postMessage(t.hwnd, wmAppOutput, 0, 0)
	}
	postClose := func(err error) {
		postMessage(t.hwnd, wmAppClosed, storeDisconnect(err), 0)
	}
	go func() {
		logDebug("connect goroutine start")
		var c *Conn
		var err error
		if isTelnet {
			c, err = ConnectTelnet(cfg, cols, rows, postOut, postClose)
		} else {
			c, err = Connect(cfg, cols, rows, confirm, postOut, postClose)
		}
		if err != nil {
			logDebug("connect done err=" + err.Error())
		} else {
			logDebug("connect done ok")
		}
		res := &connectResult{gen: gen, conn: c, err: err, term: t}
		postMessage(hwndSession, wmAppConnectDone, storeConnect(res), 0)
	}()
}
// createTermWindow selalu membuat jendela terminal BARU (bukan reuse),
// supaya tiap Connect membuka terminal sendiri dalam proses yang sama.
// Font terminal dibuat sekali saja dan dipakai bersama semua jendela.
func createTermWindow() *termWin {
	t := &termWin{mouseTrackBtn: -1}
	hwnd := win.CreateWindowEx(0, utf16("SSHTermTerminal"), utf16("KERM Terminal"),
		win.WS_OVERLAPPEDWINDOW|win.WS_VSCROLL, win.CW_USEDEFAULT, win.CW_USEDEFAULT, 1100, 650,
		0, 0, hInst, nil)
	if hwnd == 0 {
		return nil
	}
	setWindowIcon(hwnd)
	t.hwnd = hwnd
	termWins[hwnd] = t
	termOrder = append(termOrder, hwnd)
	if termFont == 0 {
		termFont = makeTermFont()
		if termFont == 0 {
			var lf win.LOGFONT
			lf.LfHeight = -19
			lf.LfWeight = 400
			lf.LfQuality = 5
			lf.LfPitchAndFamily = 0x31
			copyLFName(&lf, "Cascadia Mono")
			termFont = win.CreateFontIndirect(&lf)
		}
		hdc := win.GetDC(hwnd)
		old := win.SelectObject(hdc, win.HGDIOBJ(termFont))
		var tm win.TEXTMETRIC
		win.GetTextMetrics(hdc, &tm)
		charW = tm.TmAveCharWidth
		charH = tm.TmHeight + tm.TmExternalLeading
		win.SelectObject(hdc, old)
		win.ReleaseDC(hwnd, hdc)
		if charW <= 0 {
			charW = 9
		}
		if charH <= 0 {
			charH = 20
		}
	}
	win.SetTimer(hwnd, timerCursorBlink, 500, 0) // timer 2 (auto-scroll seleksi) hanya saat drag di luar tepi
	return t
}
func makeTermFont() win.HFONT {
	candidates := []string{"DejaVu Sans Mono", "JetBrains Mono", "Cascadia Mono", "Consolas"}
	// Buat DC sementara untuk mengecek nama font yang benar-benar dipilih.
	hdc := win.GetDC(0)
	if hdc == 0 {
		return 0
	}
	defer win.ReleaseDC(0, hdc)
	for _, name := range candidates {
		var lf win.LOGFONT
		lf.LfHeight = -19
		lf.LfWeight = 400
		lf.LfQuality = 5 // CLEARTYPE_QUALITY
		lf.LfPitchAndFamily = 0x31 // FIXED_PITCH | FF_MODERN
		copyLFName(&lf, name)
		f := win.CreateFontIndirect(&lf)
		if f == 0 {
			continue
		}
		old := win.SelectObject(hdc, win.HGDIOBJ(f))
		actual := getTextFace(hdc)
		win.SelectObject(hdc, old)
		if strings.EqualFold(strings.TrimSpace(actual), name) {
			return f
		}
		win.DeleteObject(win.HGDIOBJ(f))
	}
	return 0
}

func copyLFName(lf *win.LOGFONT, name string) {
	u, _ := windows.UTF16FromString(name)
	for i := 0; i < len(u) && i < len(lf.LfFaceName); i++ {
		lf.LfFaceName[i] = u[i]
	}
}

func brighten(c uint32) uint32 {
	for i := 0; i < 8; i++ {
		if c == palette16[i] {
			return palette16[i+8]
		}
	}
	return c
}

// dimColor menggelapkan warna fg menjadi setengah per kanal,
// seperti faint/dim PuTTY. Dipakai SGR 2 (faint) yang sebelumnya
// diabaikan (malah disalahartikan sebagai bold off).
func dimColor(c uint32) uint32 {
	r := ((c >> 16) & 0xFF) / 2
	g := ((c >> 8) & 0xFF) / 2
	b := (c & 0xFF) / 2
	return (r << 16) | (g << 8) | b
}

func cellColors(t *termWin, x, y int, cell Cell) (uint32, uint32) {
	fg := cell.Fg
	bg := cell.Bg
	if cell.FgDefault {
		fg = 0xBBBBBB
		if cell.Bold {
			fg = 0xFFFFFF
		}
	} else if cell.Bold {
		fg = brighten(fg)
	}
	if cell.BgDefault {
		bg = 0x000000
	}
	if cell.Dim {
		fg = dimColor(fg)
	}
	if cell.Reverse {
		fg, bg = bg, fg
	}
	if t != nil && t.selActive && inSelection(t, x, y) {
		fg, bg = bg, fg
	}
	// Kursor kedip: SELALU blok hijau (muda saat blink on, tua saat
	// blink off), teks hitam, apa pun sel di bawahnya. Sengaja paling
	// akhir supaya kursor menang atas reverse dan seleksi — sebelumnya
	// kursor hanya menukar warna sel, sehingga warnanya ikut berubah
	// (hijau/biru/abu-abu) tergantung karakter/spasi di bawahnya.
	if t != nil && t.scrollOffset == 0 && t.term != nil && t.term.CursorVis && x == t.term.CursorX && y == t.term.CursorY {
		fg, bg = cursorColors(t.cursorBlinkOn)
	}
	return fg, bg
}
func paintTerm(hwnd win.HWND) {
	var ps win.PAINTSTRUCT
	hdc := win.BeginPaint(hwnd, &ps)
	defer win.EndPaint(hwnd, &ps)
	t := termByHWND(hwnd)
	if t == nil || t.term == nil {
		return
	}
	term := t.term
	var rc win.RECT
	win.GetClientRect(hwnd, &rc)
	width := rc.Right - rc.Left
	height := rc.Bottom - rc.Top
	if width <= 0 || height <= 0 {
		return
	}
	memDC := win.CreateCompatibleDC(hdc)
	if memDC == 0 {
		return
	}
	defer win.DeleteDC(memDC)
	bmp := win.CreateCompatibleBitmap(hdc, width, height)
	if bmp == 0 {
		return
	}
	defer win.DeleteObject(win.HGDIOBJ(bmp))
	oldBmp := win.SelectObject(memDC, win.HGDIOBJ(bmp))
	defer win.SelectObject(memDC, oldBmp)
	oldFont := win.SelectObject(memDC, win.HGDIOBJ(termFont))
	defer win.SelectObject(memDC, oldFont)
	win.SetBkMode(memDC, win.TRANSPARENT)
	brushCache := map[uint32]win.HBRUSH{}
	defer func() {
		for _, b := range brushCache {
			win.DeleteObject(win.HGDIOBJ(b))
		}
	}()
	brushFor := func(bg uint32) win.HBRUSH {
		if b, ok := brushCache[bg]; ok {
			return b
		}
		b := createSolidBrush(colorFromUint32(bg))
		brushCache[bg] = b
		return b
	}
	lines := term.LinesForView(t.scrollOffset)
	for y, line := range lines {
		for x := 0; x < term.Cols && x < len(line); x++ {
			_, bg := cellColors(t, x, y, line[x])
			cellRc := win.RECT{Left: int32(x) * charW, Top: int32(y) * charH, Right: int32(x+1) * charW, Bottom: int32(y+1) * charH}
			fillRect(memDC, &cellRc, brushFor(bg))
		}
		runStart := -1
		var runFg uint32
		var runText []rune
		flushRun := func() {
			if runStart < 0 || len(runText) == 0 {
				runStart = -1
				runText = nil
				return
			}
			win.SetTextColor(memDC, colorFromUint32(runFg))
			u, _ := windows.UTF16FromString(string(runText))
			win.TextOut(memDC, int32(runStart)*charW, int32(y)*charH, &u[0], int32(len(u)-1))
			runStart = -1
			runText = nil
		}
		for x := 0; x < term.Cols && x < len(line); x++ {
			cell := line[x]
			fg, _ := cellColors(t, x, y, cell)
			if cell.Ch == ' ' || cell.Ch == 0 {
				flushRun()
				continue
			}
			if runStart < 0 {
				runStart = x
				runFg = fg
				runText = []rune{cell.Ch}
			} else if fg == runFg {
				runText = append(runText, cell.Ch)
			} else {
				flushRun()
				runStart = x
				runFg = fg
				runText = []rune{cell.Ch}
			}
		}
		flushRun()
		for x := 0; x < term.Cols && x < len(line); x++ {
			if !line[x].Underline {
				continue
			}
			fg, _ := cellColors(t, x, y, line[x])
			lrc := win.RECT{Left: int32(x) * charW, Top: int32(y+1)*charH - 2, Right: int32(x+1) * charW, Bottom: int32(y+1) * charH}
			fillRect(memDC, &lrc, brushFor(fg))
		}
		// Strikethrough (SGR 9): garis tengah sel, analog underline.
		for x := 0; x < term.Cols && x < len(line); x++ {
			if !line[x].Strike {
				continue
			}
			fg, _ := cellColors(t, x, y, line[x])
			src := win.RECT{Left: int32(x) * charW, Top: int32(y)*charH + charH/2, Right: int32(x+1) * charW, Bottom: int32(y)*charH + charH/2 + 1}
			fillRect(memDC, &src, brushFor(fg))
		}
	}
	win.BitBlt(hdc, 0, 0, width, height, memDC, 0, 0, win.SRCCOPY)
}
func inSelection(t *termWin, x, y int) bool {
	// y di sini adalah baris tampilan (paint loop). Ubah dulu ke
	// absolut dengan scrollOffset/scrollback saat ini, lalu bandingkan
	// dengan ujung seleksi absolut — jadi highlight tetap menempel
	// pada baris yang benar walau tampilan bergeser saat auto-scroll.
	if t == nil || t.term == nil {
		return false
	}
	absY := selectionAbsLine(t.term.ScrollbackLen(), t.term.Rows, t.scrollOffset, t.term.AltActive(), y)
	y1, x1, y2, x2 := orderedSelection(t.selStartAbsY, t.selStartX, t.selEndAbsY, t.selEndX)
	if absY < y1 || absY > y2 {
		return false
	}
	y = absY
	if y1 == y2 {
		return x >= x1 && x < x2
	}
	if y == y1 {
		return x >= x1
	}
	if y == y2 {
		return x < x2
	}
	return true
}
func cellFromMouse(t *termWin, lParam uintptr) (int, int) {
	x := int(int16(lParam & 0xFFFF))
	y := int(int16((lParam >> 16) & 0xFFFF))
	cx := x / int(charW)
	cy := y / int(charH)
	if cx < 0 {
		cx = 0
	}
	if cy < 0 {
		cy = 0
	}
	if t != nil && t.term != nil {
		if cx >= t.term.Cols {
			cx = t.term.Cols - 1
		}
		if cy >= t.term.Rows {
			cy = t.term.Rows - 1
		}
	}
	return cx, cy
}

// mouseClientPos mengembalikan koordinat mouse dalam klien jendela,
// TANPA clamp — boleh negatif / melebihi ukuran klien saat drag
// memakai capture. Dipakai khusus untuk drag seleksi + auto-scroll.
func mouseClientPos(lParam uintptr) (int, int) {
	return int(int16(lParam & 0xFFFF)), int(int16((lParam >> 16) & 0xFFFF))
}

func clampCellX(t *termWin, mouseX int) int {
	cx := 0
	if charW > 0 {
		cx = mouseX / int(charW)
	}
	if cx < 0 {
		cx = 0
	}
	if t != nil && t.term != nil && cx >= t.term.Cols {
		cx = t.term.Cols - 1
	}
	return cx
}

// selectionEndFromMouse menetapkan ujung seleksi dari posisi mouse
// saat ini (di dalam jendela): kolom di-clamp, baris diubah ke absolut
// dengan scrollOffset saat ini. Kembali false kalau tidak ada terminal.
func selectionEndFromMouse(t *termWin, mouseX, mouseY int) bool {
	if t == nil || t.term == nil || charH <= 0 {
		return false
	}
	cy := mouseY / int(charH)
	if cy < 0 {
		cy = 0
	}
	if cy >= t.term.Rows {
		cy = t.term.Rows - 1
	}
	t.selEndX = clampCellX(t, mouseX)
	t.selEndAbsY = selectionAbsLine(t.term.ScrollbackLen(), t.term.Rows, t.scrollOffset, t.term.AltActive(), cy)
	return true
}

const (
	timerCursorBlink  = 1 // sudah ada sejak awal, 500 ms di createTermWindow
	timerSelAutoScroll = 2 // auto-scroll drag seleksi, 50 ms, hanya saat drag di luar tepi
)

func startSelAutoScroll(t *termWin) {
	if t == nil || t.hwnd == 0 || t.selAutoScroll {
		return
	}
	win.SetTimer(t.hwnd, timerSelAutoScroll, 50, 0)
	t.selAutoScroll = true
}

func stopSelAutoScroll(t *termWin) {
	if t == nil || !t.selAutoScroll {
		return
	}
	t.selAutoScroll = false
	if t.hwnd != 0 {
		win.KillTimer(t.hwnd, timerSelAutoScroll)
	}
}

// autoScrollSelectionStep melakukan satu tahap auto-scroll saat drag
// melewati tepi vertikal: geser tampilan, lalu tetapkan ujung seleksi
// ke baris tepi tampilan yang baru (atas kalau drag ke atas, bawah
// kalau drag ke bawah) dengan kolom mouse saat ini. Kembali true
// kalau tampilan benar-benar bergeser (false = mouse di dalam, alt
// screen, tidak ada scrollback, atau sudah mentok di ujung).
func autoScrollSelectionStep(t *termWin, mouseX, mouseY int) bool {
	if t == nil || t.term == nil || t.hwnd == 0 || t.term.AltActive() {
		return false
	}
	var rc win.RECT
	win.GetClientRect(t.hwnd, &rc)
	height := int(rc.Bottom - rc.Top)
	if height <= 0 || charH <= 0 {
		return false
	}
	delta := autoScrollDelta(mouseY, height, int(charH))
	if delta == 0 {
		return false
	}
	oldOffset := t.scrollOffset
	setScrollOffset(t, t.scrollOffset+delta)
	if t.scrollOffset == oldOffset {
		return false
	}
	// Ujung seleksi = baris tepi tampilan yang baru.
	edgeViewY := 0
	if delta < 0 {
		edgeViewY = t.term.Rows - 1
	}
	t.selEndX = clampCellX(t, mouseX)
	t.selEndAbsY = selectionAbsLine(t.term.ScrollbackLen(), t.term.Rows, t.scrollOffset, t.term.AltActive(), edgeViewY)
	t.selActive = true
	win.InvalidateRect(t.hwnd, nil, false)
	return true
}

func termWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	t := termByHWND(hwnd)
	switch msg {
	case win.WM_ERASEBKGND:
		return 1
	case win.WM_SIZE:
		// Bug minimize 2026-10-29: saat minimize, Windows mengirim
		// WM_SIZE dengan ukuran klien 0x0. Kalau applyTermSize tetap
		// dipanggil, buffer terpotong jadi 20x5 dan sebagian tulisan
		// hilang permanen saat restore. Minimize bukan perubahan
		// ukuran terminal, jadi lewati total (buffer, scrollback,
		// dan PTY server tidak berubah). Restore ke ukuran semula
		// kemudian akan no-op (Resize ukuran sama kembali lebih awal).
		if t != nil && !shouldSkipTermResize(wParam) {
			applyTermSize(t)
			clampScrollOffset(t)
			updateTermScrollbar(t)
		}
		win.InvalidateRect(hwnd, nil, false)
		return 0
	case win.WM_PAINT:
		paintTerm(hwnd)
		return 0
	case wmAppOutput:
		if t == nil {
			return 0
		}
		if t.disconnected {
			// Koneksi sudah putus dan banner keterangan sudah
			// ditampilkan: output nyasar dari koneksi lama tidak
			// boleh menimpa / menggeser banner. Buang saja.
			t.outMu.Lock()
			t.outQueue = nil
			t.outMu.Unlock()
			return 0
		}
		t.outMu.Lock()
		q := t.outQueue
		t.outQueue = nil
		t.outMu.Unlock()
		if t.term != nil {
			sbBefore := t.term.ScrollbackLen()
			offBefore := t.scrollOffset
			for _, chunk := range q {
				resp := t.term.Feed(chunk)
				if len(resp) > 0 {
					writeToSSH(t, resp)
				}
			}
			if offBefore > 0 {
				t.scrollOffset = offBefore + (t.term.ScrollbackLen() - sbBefore)
				clampScrollOffset(t)
			}
			updateTermScrollbar(t)
		}
		win.InvalidateRect(hwnd, nil, false)
		return 0
	case wmAppClosed:
		logDebug("wmAppClosed received (session ended)")
		if t == nil {
			return 0
		}
		info, ok := takeDisconnect(wParam)
		if !ok {
			// Stale message / no reason ID: do not show a false
			// MessageBox, treat as a clean disconnect.
			info = disconnectInfo{clean: true, reason: "Connection Closed"}
		}
		if t.disconnected {
			// Sudah pernah ditangani (duplikat): abaikan.
			return 0
		}
		connMu.Lock()
		c := t.conn
		t.conn = nil
		connMu.Unlock()
		if c != nil {
			c.Close()
		}
		who := termWho(t)
		logDebug("disconnect who=" + who + " reason=" + info.reason + " detail=" + info.detail)
		if info.clean {
			// Clean disconnect (exit/logout, or local close):
			// terminal closes, panel reopens, status log gains
			// one disconnect line.
			destroyTermWindow(t)
			win.ShowWindow(hwndSession, win.SW_SHOW)
			appendStatus(disconnectLine(who))
			win.SetFocus(hwndSession)
			return 0
		}
		// Abnormal disconnect (network, sleep, server restart, reset,
		// timeout): do NOT destroy the terminal silently — that was
		// the reported bug (laptop sleep, wake up, terminal gone
		// with no explanation). The terminal stays open with the
		// restored full 3-line notice, its title marks it
		// DISCONNECTED, the status log gains one short disconnect
		// line, and a MessageBox shows the full 3-line notice.
		// Close this terminal yourself (WM_CLOSE) to return to the
		// panel, then Connect again. Scrollback, selection, and
		// copy still work; typing/pasting to the dead server is
		// blocked by the guards in sendToSSH/pasteFromClipboard.
		t.disconnected = true
		t.disconnReason = info.reason
		t.disconnDetail = info.detail
		t.disconnTime = time.Now()
		// SFTP ikut ditutup: client SFTP memakai koneksi yang sama
		// yang baru saja putus, jadi jendelanya tidak lagi berguna.
		closeSFTPForTerm(t)
		updateSFTPButton()
		stopSelAutoScroll(t)
		t.selDragging = false
		t.scrollOffset = 0
		timeStr := t.disconnTime.Format("2006-01-02 15:04:05")
		notice := disconnectNotice(who, info.reason, timeStr)
		if t.term != nil {
			t.term.Feed([]byte("\r\n" + notice + "\r\n"))
		}
		clampScrollOffset(t)
		updateTermScrollbar(t)
		setWindowText(hwnd, "KERM Terminal - "+who+" (DISCONNECTED)")
		appendStatus(disconnectLine(who))
		// Restore if it was minimized during sleep, then bring to
		// front so the notice is not hidden behind other windows.
		win.ShowWindow(hwnd, win.SW_RESTORE)
		win.SetForegroundWindow(hwnd)
		win.SetFocus(hwnd)
		win.InvalidateRect(hwnd, nil, false)
		msgBox(hwnd, notice, "Disconnected", win.MB_ICONWARNING|win.MB_OK)
		return 0
	case win.WM_TIMER:
		if t == nil {
			return 0
		}
		// PENTING: bedakan ID timer. Handler lama mengabaikan wParam,
		// jadi timer auto-scroll 50 ms akan ikut membalik kursor
		// kedip 20x/detik kalau tidak dibedakan.
		if wParam == timerSelAutoScroll {
			// Auto-scroll drag seleksi: mouse masih di luar tepi?
			// Kalau drag sudah selesai / mouse sudah kembali ke
			// dalam / sudah mentok, hentikan timer.
			if !t.selDragging || t.term == nil {
				stopSelAutoScroll(t)
				return 0
			}
			var pt win.POINT
			if !win.GetCursorPos(&pt) || !win.ScreenToClient(hwnd, &pt) {
				stopSelAutoScroll(t)
				return 0
			}
			if !autoScrollSelectionStep(t, int(pt.X), int(pt.Y)) {
				stopSelAutoScroll(t)
			}
			return 0
		}
		if wParam != timerCursorBlink {
			return 0
		}
		t.cursorBlinkOn = !t.cursorBlinkOn
		if t.term != nil && t.scrollOffset == 0 && t.term.CursorVis {
			rc := win.RECT{Left: int32(t.term.CursorX) * charW, Top: int32(t.term.CursorY) * charH, Right: int32(t.term.CursorX+1) * charW, Bottom: int32(t.term.CursorY+1) * charH}
			win.InvalidateRect(hwnd, &rc, false)
		}
		return 0
	case win.WM_CAPTURECHANGED:
		// Capture hilang di luar kendali drag (mis. Alt+Tab): akhiri
		// drag tanpa auto-copy, tapi pertahankan seleksi kalau sudah
		// ada — user masih bisa copy dengan klik kanan / Ctrl+Shift+C.
		if t != nil && t.selDragging {
			stopSelAutoScroll(t)
			t.selDragging = false
			if t.selStartAbsY == t.selEndAbsY && t.selStartX == t.selEndX {
				t.selActive = false
			} else {
				t.selActive = true
			}
			win.InvalidateRect(hwnd, nil, false)
		}
		return 0
	case win.WM_SETFOCUS:
		if t != nil {
			t.cursorBlinkOn = true
		}
		win.InvalidateRect(hwnd, nil, false)
		return 0
	case win.WM_CHAR:
		if t == nil {
			return 0
		}
		if t.pwActive {
			if wParam == 0x16 {
				pastePasswordFromClipboard(t)
				return 0
			}
			if wParam == 0x03 {
				if win.GetKeyState(win.VK_SHIFT) < 0 {
					copySelectionToClipboard(t)
				} else {
					cancelPasswordPrompt(t)
				}
				return 0
			}
			r := rune(wParam)
			if r == '\r' || r == '\n' {
				finishPasswordPrompt(t)
				return 0
			}
			if r == 0x08 || r == 0x7f {
				if len(t.pwBuf) > 0 {
					t.pwBuf = t.pwBuf[:len(t.pwBuf)-1]
					t.term.Feed([]byte("\x1b[D \x1b[D"))
					win.InvalidateRect(hwnd, nil, false)
				}
				return 0
			}
			if r < 0x20 || r == 0 {
				return 0
			}
			t.pwBuf = append(t.pwBuf, r)
			t.term.Feed([]byte("*"))
			win.InvalidateRect(hwnd, nil, false)
			return 0
		}
		if wParam == 0x0E && win.GetKeyState(win.VK_SHIFT) < 0 && win.GetKeyState(win.VK_CONTROL) < 0 {
			win.ShowWindow(hwndSession, win.SW_SHOW)
			win.SetFocus(hwndSession)
			return 0
		}
		// Ctrl+Shift+S = buka SFTP untuk terminal ini ("SFTP here").
		// Ctrl+S polos (tanpa Shift) tetap ke server (XOFF/flow
		// control), jadi cek Shift wajib ada di sini.
		if wParam == 0x13 && win.GetKeyState(win.VK_SHIFT) < 0 && win.GetKeyState(win.VK_CONTROL) < 0 {
			openSFTPForTerm(t)
			return 0
		}
		if wParam == 0x16 {
			pasteFromClipboard(t)
			return 0
		}
		if wParam == 0x03 && win.GetKeyState(win.VK_SHIFT) < 0 {
			copySelectionToClipboard(t)
			return 0
		}
		r := rune(wParam)
		// Shift+Tab = Backtab (ESC[Z) seperti PuTTY/xterm, bukan Tab.
		// Ctrl+Space = NUL (0x00) — dulu dibuang oleh cek r==0 di
		// sini, padahal PuTTY mengirim NUL (dipakai vim untuk
		// completion, tmux, dll). keyCharToSend(0) = []byte{0}.
		if r == '\t' {
			sendToSSH(t, tabSeq(win.GetKeyState(win.VK_SHIFT) < 0))
			return 0
		}
		// Backspace (0x08 dari Windows) dipetakan ke DEL (0x7F)
		// seperti PuTTY — lihat keys.go. Tanpa ini, pty Linux
		// (erase default DEL, terminfo kbs=\177) tidak mengenali
		// 0x08 sebagai hapus dan malah meng-echo "^H" harfiah.
		sendToSSH(t, keyCharToSend(r))
		return 0
	case win.WM_SYSCHAR:
		// Alt+Space = menu sistem Windows (seperti PuTTY), BUKAN
		// Meta+Space ke server. Sebelum perbaikan ini, Alt+Space
		// mengirim ESC+spasi ke server dan menu sistem tidak muncul.
		// DefWindowProc membuka menu sistem itu.
		if wParam == ' ' {
			return win.DefWindowProc(hwnd, msg, wParam, lParam)
		}
		// Alt+huruf (Meta) seperti PuTTY: ESC + huruf. Sebelumnya
		// tidak ada handler WM_SYSCHAR sama sekali, jadi Alt+B/F
		// dll tidak mengirim apa-apa. Password prompt & terminal
		// putus tidak mengirim (guard di sendToSSH / pwActive).
		if t == nil || t.pwActive {
			return 0
		}
		if wParam == 0x7F {
			// Alt+Backspace = ESC + DEL (Backspace KERM = DEL).
			sendToSSH(t, []byte{0x1B, 0x7F})
			return 0
		}
		if seq := metaSeq(rune(wParam)); seq != nil {
			sendToSSH(t, seq)
		}
		return 0
	case win.WM_KEYDOWN:
		if t == nil {
			return 0
		}
		vk := int32(wParam)
		shift := win.GetKeyState(win.VK_SHIFT) < 0
		ctrl := win.GetKeyState(win.VK_CONTROL) < 0
		alt := win.GetKeyState(win.VK_MENU) < 0
		appCursor := t.term != nil && t.term.AppCursor
		shiftOnly := shift && !ctrl && !alt
		// Shift+Home/End/PgUp/PgDn di layar utama tetap scrollback
		// lokal (fitur KERM yang sudah ada); di alt screen (vim/
		// htop, scrollOffset selalu 0) Shift+… dikirim ke aplikasi
		// sebagai tombol bermodifier, karena scroll lokal di sana
		// tidak ada gunanya. Modifier lain selalu dikirim.
		altActive := t.term != nil && t.term.AltActive()
		var seq []byte
		switch vk {
		case win.VK_UP:
			seq = arrowSeq('A', appCursor, shift, ctrl, alt)
		case win.VK_DOWN:
			seq = arrowSeq('B', appCursor, shift, ctrl, alt)
		case win.VK_RIGHT:
			seq = arrowSeq('C', appCursor, shift, ctrl, alt)
		case win.VK_LEFT:
			seq = arrowSeq('D', appCursor, shift, ctrl, alt)
		case win.VK_HOME:
			if shiftOnly && !altActive && t.term != nil {
				setScrollOffset(t, t.term.ScrollbackLen())
				return 0
			}
			seq = homeEndSeq('H', appCursor, shift, ctrl, alt)
		case win.VK_END:
			if shiftOnly && !altActive && t.term != nil {
				setScrollOffset(t, 0)
				return 0
			}
			seq = homeEndSeq('F', appCursor, shift, ctrl, alt)
		case win.VK_PRIOR:
			if shiftOnly && !altActive && t.term != nil {
				setScrollOffset(t, t.scrollOffset+t.term.Rows)
				return 0
			}
			seq = tildeSeq(5, shift, ctrl, alt)
		case win.VK_NEXT:
			if shiftOnly && !altActive && t.term != nil {
				setScrollOffset(t, t.scrollOffset-t.term.Rows)
				return 0
			}
			seq = tildeSeq(6, shift, ctrl, alt)
		case win.VK_DELETE:
			seq = tildeSeq(3, shift, ctrl, alt)
		case win.VK_INSERT:
			// Seperti PuTTY/Windows: Shift+Insert = paste,
			// Ctrl+Insert = copy seleksi. Insert polos (dan
			// kombinasi lain) tetap dikirim ke aplikasi.
			if shift && !ctrl && !alt {
				pasteFromClipboard(t)
				return 0
			}
			if ctrl && !alt {
				copySelectionToClipboard(t)
				return 0
			}
			seq = tildeSeq(2, shift, ctrl, alt)
		case win.VK_F1:
			seq = fKeySeq('P', shift, ctrl, alt)
		case win.VK_F2:
			seq = fKeySeq('Q', shift, ctrl, alt)
		case win.VK_F3:
			seq = fKeySeq('R', shift, ctrl, alt)
		case win.VK_F4:
			seq = fKeySeq('S', shift, ctrl, alt)
		case win.VK_F5:
			seq = tildeSeq(15, shift, ctrl, alt)
		case win.VK_F6:
			seq = tildeSeq(17, shift, ctrl, alt)
		case win.VK_F7:
			seq = tildeSeq(18, shift, ctrl, alt)
		case win.VK_F8:
			seq = tildeSeq(19, shift, ctrl, alt)
		case win.VK_F9:
			seq = tildeSeq(20, shift, ctrl, alt)
		case win.VK_F10:
			seq = tildeSeq(21, shift, ctrl, alt)
		case win.VK_F11:
			seq = tildeSeq(23, shift, ctrl, alt)
		case win.VK_F12:
			seq = tildeSeq(24, shift, ctrl, alt)
		}
		if seq != nil {
			sendToSSH(t, seq)
			return 0
		}
		return 0
	case win.WM_MOUSEWHEEL:
		if t == nil || t.term == nil {
			return 0
		}
		delta := int(int16((wParam >> 16) & 0xFFFF))
		// Mouse reporting aktif & Shift tidak ditekan: wheel
		// dikirim sebagai tombol 64 (naik) / 65 (turun) ke
		// aplikasi, 1 event per notch (bukan 3) — vim/tmux
		// sendiri yang menentukan berapa baris per event.
		// Shift+wheel tetap scroll lokal / panah seperti biasa.
		if mouseTrackingActive(t) && win.GetKeyState(win.VK_SHIFT) >= 0 {
			// lParam wheel = koordinat layar, konversi ke klien.
			var pt win.POINT
			pt.X = int32(int16(lParam & 0xFFFF))
			pt.Y = int32(int16((lParam >> 16) & 0xFFFF))
			if win.ScreenToClient(hwnd, &pt) {
				cx, cy := cellFromMouse(t, uintptr(uint32(uint16(pt.X))|uint32(uint16(pt.Y))<<16))
				t.wheelRemainder += delta
				notches := t.wheelRemainder / 120
				t.wheelRemainder -= notches * 120
				for i := 0; i < notches; i++ {
					sendMouse(t, 64, cx, cy, false, false)
				}
				for i := 0; i < -notches; i++ {
					sendMouse(t, 65, cx, cy, false, false)
				}
			}
			return 0
		}
		if t.term.AltActive() {
			appCursor := t.term.AppCursor
			t.wheelRemainder += delta
			notches := t.wheelRemainder / 120
			t.wheelRemainder -= notches * 120
			if notches > 0 {
				for i := 0; i < notches*3; i++ {
					sendToSSH(t, arrowSeq('A', appCursor, false, false, false))
				}
			} else if notches < 0 {
				for i := 0; i < -notches*3; i++ {
					sendToSSH(t, arrowSeq('B', appCursor, false, false, false))
				}
			}
			return 0
		}
		t.wheelRemainder += delta
		notches := t.wheelRemainder / 120
		t.wheelRemainder -= notches * 120
		setScrollOffset(t, t.scrollOffset+notches*3)
		return 0
	case win.WM_VSCROLL:
		if t == nil || t.term == nil || t.term.AltActive() {
			return 0
		}
		code := int(wParam & 0xFFFF)
		thumbPos := int(int16((wParam >> 16) & 0xFFFF))
		switch code {
		case win.SB_LINEUP:
			setScrollOffset(t, t.scrollOffset+1)
		case win.SB_LINEDOWN:
			setScrollOffset(t, t.scrollOffset-1)
		case win.SB_PAGEUP:
			setScrollOffset(t, t.scrollOffset+t.term.Rows)
		case win.SB_PAGEDOWN:
			setScrollOffset(t, t.scrollOffset-t.term.Rows)
		case win.SB_THUMBTRACK, win.SB_THUMBPOSITION:
			setScrollOffset(t, t.term.ScrollbackLen()-thumbPos)
		case win.SB_TOP:
			setScrollOffset(t, t.term.ScrollbackLen())
		case win.SB_BOTTOM:
			setScrollOffset(t, 0)
		}
		return 0
	case win.WM_RBUTTONDOWN:
		if t == nil {
			return 0
		}
		// Mouse reporting: klik kanan ke aplikasi, kecuali Shift
		// (Shift+klik = copy/paste lokal seperti PuTTY).
		if mouseTrackingActive(t) && win.GetKeyState(win.VK_SHIFT) >= 0 {
			cx, cy := cellFromMouse(t, lParam)
			sendMouse(t, 2, cx, cy, false, false)
			t.mouseTrackBtn = 2
			win.SetCapture(hwnd)
			win.SetFocus(hwnd)
			return 0
		}
		win.SetFocus(hwnd)
		if t.pwActive {
			pastePasswordFromClipboard(t)
			return 0
		}
		if t.selActive && (t.selStartX != t.selEndX || t.selStartAbsY != t.selEndAbsY) {
			copySelectionToClipboard(t)
			t.selActive = false
			t.selDragging = false
			stopSelAutoScroll(t)
			win.InvalidateRect(hwnd, nil, false)
		} else {
			pasteFromClipboard(t)
		}
		return 0
	case win.WM_RBUTTONUP:
		if t != nil && t.mouseTrackBtn == 2 {
			cx, cy := cellFromMouse(t, lParam)
			sendMouse(t, 2, cx, cy, true, false)
			t.mouseTrackBtn = -1
			win.ReleaseCapture()
		}
		return 0
	case win.WM_MBUTTONDOWN:
		if t == nil {
			return 0
		}
		// Mouse reporting: klik tengah ke aplikasi, kecuali Shift.
		// Tanpa mouse reporting, klik tengah = paste (PuTTY).
		if mouseTrackingActive(t) && win.GetKeyState(win.VK_SHIFT) >= 0 {
			cx, cy := cellFromMouse(t, lParam)
			sendMouse(t, 1, cx, cy, false, false)
			t.mouseTrackBtn = 1
			win.SetCapture(hwnd)
			win.SetFocus(hwnd)
			return 0
		}
		win.SetFocus(hwnd)
		if !t.pwActive {
			pasteFromClipboard(t)
		}
		return 0
	case win.WM_MBUTTONUP:
		if t != nil && t.mouseTrackBtn == 1 {
			cx, cy := cellFromMouse(t, lParam)
			sendMouse(t, 1, cx, cy, true, false)
			t.mouseTrackBtn = -1
			win.ReleaseCapture()
		}
		return 0
	case win.WM_LBUTTONDOWN:
		if t == nil {
			return 0
		}
		// Mouse reporting: klik ke aplikasi, kecuali Shift
		// (Shift+drag = seleksi lokal seperti PuTTY) atau saat
		// membaca scrollback (mouseTrackingActive sudah false).
		if mouseTrackingActive(t) && win.GetKeyState(win.VK_SHIFT) >= 0 {
			cx, cy := cellFromMouse(t, lParam)
			sendMouse(t, 0, cx, cy, false, false)
			t.mouseTrackBtn = 0
			win.SetCapture(hwnd)
			win.SetFocus(hwnd)
			return 0
		}
		cx, cy := cellFromMouse(t, lParam)
		t.selStartX = cx
		t.selEndX = cx
		if t.term != nil {
			t.selStartAbsY = selectionAbsLine(t.term.ScrollbackLen(), t.term.Rows, t.scrollOffset, t.term.AltActive(), cy)
			t.selEndAbsY = t.selStartAbsY
		}
		t.selDragging = true
		t.selActive = false
		stopSelAutoScroll(t)
		// Capture: tanpa ini, drag yang keluar dari jendela berhenti
		// menerima WM_MOUSEMOVE — inilah salah satu penyebab blok
		// panjang tidak bisa dipilih sampai selesai.
		win.SetCapture(hwnd)
		win.SetFocus(hwnd)
		return 0
	case win.WM_MOUSEMOVE:
		// Mouse reporting: drag (mode 1002/1003) atau gerakan
		// bebas (mode 1003 saja) dikirim ke aplikasi. Mode klik
		// saja (1000) tidak mengirim gerakan. Kalau tidak ada
		// tombol tracking yang ditekan dan bukan mode 1003,
		// tidak ada yang dikirim.
		if t != nil && !t.selDragging && mouseTrackingActive(t) {
			if t.mouseTrackBtn >= 0 && t.term.MouseMode >= 2 {
				cx, cy := cellFromMouse(t, lParam)
				sendMouse(t, t.mouseTrackBtn, cx, cy, false, true)
				return 0
			}
			if t.mouseTrackBtn < 0 && t.term.MouseMode == 3 {
				cx, cy := cellFromMouse(t, lParam)
				sendMouse(t, 3, cx, cy, false, true)
				return 0
			}
		}
		if t != nil && t.selDragging {
			mx, my := mouseClientPos(lParam)
			// Di luar tepi vertikal: satu tahap auto-scroll langsung
			// (responsif, tidak menunggu tick timer), lalu timer
			// 50 ms meneruskan scroll selama mouse tetap di luar.
			// Di dalam jendela: hentikan timer, ujung seleksi =
			// posisi mouse.
			if autoScrollSelectionStep(t, mx, my) {
				startSelAutoScroll(t)
			} else {
				// autoScrollSelectionStep false bisa berarti mouse
				// di dalam (normal), atau sudah mentok / alt screen.
				// Kalau mouse di dalam, update ujung normal; kalau
				// mentok di luar, ujung tetap di tepi terakhir.
				var rc win.RECT
				win.GetClientRect(hwnd, &rc)
				if my >= 0 && my < int(rc.Bottom-rc.Top) {
					stopSelAutoScroll(t)
					selectionEndFromMouse(t, mx, my)
					t.selActive = true
					win.InvalidateRect(hwnd, nil, false)
				} else {
					// Sudah mentok: hentikan timer supaya tidak
					// berputar sia-sia, tapi jangan ubah ujung.
					stopSelAutoScroll(t)
				}
			}
		}
		return 0
	case win.WM_LBUTTONUP:
		if t == nil {
			return 0
		}
		if t.mouseTrackBtn == 0 {
			cx, cy := cellFromMouse(t, lParam)
			sendMouse(t, 0, cx, cy, true, false)
			t.mouseTrackBtn = -1
			win.ReleaseCapture()
			return 0
		}
		if t.selDragging {
			// Ujung akhir dari posisi mouse saat lepas (di-clamp ke
			// tepi kalau lepas di luar jendela — sama dengan tepi
			// auto-scroll terakhir, jadi tidak menggeser seleksi).
			mx, my := mouseClientPos(lParam)
			selectionEndFromMouse(t, mx, my)
			t.selDragging = false
			stopSelAutoScroll(t)
			if t.selStartX != t.selEndX || t.selStartAbsY != t.selEndAbsY {
				t.selActive = true
				copySelectionToClipboard(t)
			} else {
				t.selActive = false
			}
			win.InvalidateRect(hwnd, nil, false)
			win.ReleaseCapture()
		}
		return 0
	case win.WM_CLOSE:
		// Tutup terminal -> disconnect terminal INI saja, hancurkan
		// jendelanya, lalu panel sesi utama DIBUKA LAGI — meski
		// panel sempat ditutup user saat terminal masih jalan.
		// Terminal lain (kalau ada) tidak terganggu, proses tidak
		// keluar. Kalau sedang di password prompt, tutup = batal
		// prompt (terminal dihancurkan, panel dibuka lagi).
		if t == nil {
			return 0
		}
		t.pwActive = false
		t.pwBuf = nil
		connMu.Lock()
		c := t.conn
		t.conn = nil
		connMu.Unlock()
		if c != nil {
			c.Close()
		}
		if pendingTerm == t {
			pendingTerm = nil
		}
		wasConnected := t.disconnected || c != nil
		disconnWho := termWho(t)
		destroyTermWindow(t)
		win.ShowWindow(hwndSession, win.SW_SHOW)
		if wasConnected {
			// Terminal closed (by the user, or after a disconnect):
			// status log gains one disconnect line.
			appendStatus(disconnectLine(disconnWho))
		}
		win.SetFocus(hwndSession)
		return 0
	case win.WM_DESTROY:
		delete(termWins, hwnd)
		removeTermOrder(hwnd)
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}
var (
	sessionProc = syscall.NewCallback(sessionWndProc)
	termProc    = syscall.NewCallback(termWndProc)
)

func makeUIFont(name string, height int32, weight int32) win.HFONT {
	var lf win.LOGFONT
	lf.LfHeight = height
	lf.LfWeight = weight
	lf.LfQuality = 5 // CLEARTYPE_QUALITY
	copyLFName(&lf, name)
	return win.CreateFontIndirect(&lf)
}

func main() {
	// PERBAIKAN NOT RESPONDING #2 (yang paling mungkin): aplikasi GUI di Go
	// WAJIB mengunci goroutine utama ke satu OS thread. Tanpa ini, scheduler
	// Go bisa memindahkan goroutine utama ke thread lain di tengah jalan,
	// sementara jendela Windows terikat ke thread pembuatnya — antrean pesan
	// thread lama tidak lagi dipompa, jendela tampak Not Responding, dan
	// pesan konfirmasi host key / selesai koneksi tidak pernah diproses
	// (goroutine koneksi menunggu konfirmasi selamanya = deadlock).
	// Inilah yang kemungkinan terjadi di versi sebelumnya, meskipun koneksi
	// sudah dipindah ke background thread.
	runtime.LockOSThread()
	logDebug("main start (LockOSThread ok)")
	// Font UI modern (Segoe UI, bukan font dialog klasik) + warna tema.
	uiFont = makeUIFont("Segoe UI", -16, 400)
	uiFontBold = makeUIFont("Segoe UI", -16, 700)
	headerFont = makeUIFont("Segoe UI", -27, 700)
	headerSubFont = makeUIFont("Segoe UI", -15, 400)
	sessionBgBrush = createSolidBrush(rgb(0xF2, 0xF4, 0xF7))
	accentBrush = createSolidBrush(rgb(0x1F, 0x4E, 0x79))
	termBgBrush = createSolidBrush(rgb(0x00, 0x00, 0x00)) // Termux: hitam pekat
	// Icon aplikasi/jendela: embedded + override icon.ico (2026-10-29).
	// Harus sebelum RegisterClassEx supaya HIcon kelas sudah benar.
	initAppIcon()
	// Session window class
	sessionClass := win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		Style:         win.CS_HREDRAW | win.CS_VREDRAW,
		LpfnWndProc:   sessionProc,
		HInstance:     hInst,
		HIcon:         appIcon,
		HIconSm:       appIconSm,
		HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
		HbrBackground: sessionBgBrush,
		LpszClassName: utf16("SSHTermSession"),
	}
	win.RegisterClassEx(&sessionClass)
	// Terminal window class
	termClass := win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		Style:         win.CS_HREDRAW | win.CS_VREDRAW,
		LpfnWndProc:   termProc,
		HInstance:     hInst,
		HIcon:         appIcon,
		HIconSm:       appIconSm,
		HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
		HbrBackground: termBgBrush,
		LpszClassName: utf16("SSHTermTerminal"),
	}
	win.RegisterClassEx(&termClass)
	// SFTP window class (2026-10-30): jendela file manager terpisah,
	// dua panel lokal/remote. Progress bar butuh common controls.
	win.InitCommonControlsEx(&win.INITCOMMONCONTROLSEX{DwSize: uint32(unsafe.Sizeof(win.INITCOMMONCONTROLSEX{})), DwICC: win.ICC_PROGRESS_CLASS | win.ICC_LISTVIEW_CLASSES})
	sftpClass := win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		Style:         win.CS_HREDRAW | win.CS_VREDRAW,
		LpfnWndProc:   sftpProc,
		HInstance:     hInst,
		HIcon:         appIcon,
		HIconSm:       appIconSm,
		HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
		HbrBackground: sessionBgBrush,
		LpszClassName: utf16("SSHTermSFTP"),
	}
	win.RegisterClassEx(&sftpClass)
	// Input dialog class (SFTP Rename / New folder).
	inputClass := win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		Style:         win.CS_HREDRAW | win.CS_VREDRAW,
		LpfnWndProc:   inputProc,
		HInstance:     hInst,
		HIcon:         appIcon,
		HIconSm:       appIconSm,
		HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
		HbrBackground: sessionBgBrush,
		LpszClassName: utf16("SSHTermInput"),
	}
	win.RegisterClassEx(&inputClass)
	// Pick dialog class (pemilih terminal untuk tombol SFTP panel).
	pickClass := win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		Style:         win.CS_HREDRAW | win.CS_VREDRAW,
		LpfnWndProc:   pickProc,
		HInstance:     hInst,
		HIcon:         appIcon,
		HIconSm:       appIconSm,
		HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
		HbrBackground: sessionBgBrush,
		LpszClassName: utf16("SSHTermPick"),
	}
	win.RegisterClassEx(&pickClass)

	hwndSession = win.CreateWindowEx(0, utf16("SSHTermSession"), utf16("Kerm - SSH & Telnet terminal"),
		win.WS_OVERLAPPEDWINDOW, win.CW_USEDEFAULT, win.CW_USEDEFAULT, 580, 700,
		0, 0, hInst, nil)
	if hwndSession == 0 {
		return
	}
	setWindowIcon(hwndSession)
	// Satu proses, 1 panel sesi, banyak terminal (2026-10-29):
	// panel sesi selalu ditampilkan di awal sebagai satu-satunya
	// panel pemilihan. Tidak ada lagi proses sekunder / argumen
	// -new-session — terminal baru dibuat di proses yang sama.
	win.ShowWindow(hwndSession, win.SW_SHOW)
	win.UpdateWindow(hwndSession)
	logDebug("session window shown")
	win.SetFocus(ctlSessionName)
	var msg win.MSG
	for win.GetMessage(&msg, 0, 0, 0) != 0 {
		// Tab di jendela SFTP: pindah fokus antar kontrol SFTP
		// (handler manual, bukan IsDialogMessage, supaya Enter di
		// path edit tidak salah jadi klik tombol default).
		if sftpHandleTab(&msg) {
			continue
		}
		// Enter di Edit path SFTP (lokal / remote) = buka directory itu.
		if sftpHandlePathEnter(&msg) {
			continue
		}
		// Tab / Enter / Escape di dialog input SFTP (Rename / New folder).
		if inputHandleKey(&msg) {
			continue
		}
		// Tab / Enter / Escape di dialog pemilih terminal SFTP.
		if pickHandleKey(&msg) {
			continue
		}
		// Hanya untuk pesan yang ditujukan ke jendela sesi (bukan
		// terminal): Tab/Shift+Tab untuk pindah kolom, Enter untuk
		// tombol Connect (default button).
		// PENTING MULTI-SESI: dulu panel sesi selalu disembunyikan
		// saat terminal connected, jadi IsWindowVisible saja cukup
		// membedakan sesi vs terminal. Sekarang panel sesi TETAP
		// terlihat saat terminal connected, jadi visibilitas saja
		// TIDAK cukup — tanpa pengecekan target pesan ini, Enter/
		// Atas/Bawah di terminal akan salah dikira Enter/Atas/Bawah
		// di panel sesi (Enter di shell malah membuka sesi baru,
		// panah Atas malah mengganti pilihan sesi, bukan history
		// shell). Jadi dialog logic hanya jalan kalau msg.HWnd
		// adalah panel sesi atau anaknya; pesan terminal langsung
		// ke Translate/Dispatch seperti biasa, sehingga Tab
		// (completion), Enter (jalankan perintah), dan panah
		// (history) di shell tetap bekerja.
		if hwndSession != 0 && win.IsWindowVisible(hwndSession) && (msg.HWnd == hwndSession || win.IsChild(hwndSession, msg.HWnd)) {
			// BUG SEBELUMNYA: Enter tidak selalu Connect. IsDialogMessage
			// + tombol default saja tidak cukup saat fokus ada di daftar
			// sesi (ListBox menelan Enter tanpa memicu tombol default).
			// Perbaikan: tangkap Enter di sini dan panggil doConnect
			// langsung, kecuali fokus ada di tombol lain (Simpan/Hapus/
			// New/Browse/Batal — Enter di sana harus mengklik tombol itu)
			// atau di ComboBox yang dropdown-nya sedang terbuka (Enter =
			// pilih item dropdown). Selama belum ada koneksi berjalan
			// (tombol Connect masih aktif), Enter = Connect, apa pun
			// kolomnya, termasuk setelah pilih sesi pakai Atas/Bawah.
			// Tab membatalkan flag pilih-sesi-lewat-keyboard: kalau user
			// sengaja Tab ke tombol lain lalu Enter, tombol itu yang
			// harus diklik, bukan Connect.
			if msg.Message == win.WM_KEYDOWN && msg.WParam == uintptr(win.VK_TAB) {
				sessionSelectedViaKeys = false
			}
			if msg.Message == win.WM_KEYDOWN && msg.WParam == uintptr(win.VK_RETURN) {
				// Kalau sesi baru saja dipilih lewat Up/Down, Enter =
				// Connect, apa pun tombol yang tadinya fokus.
				if sessionSelectedViaKeys && win.IsWindowEnabled(win.GetDlgItem(hwndSession, idConnect)) {
					sessionSelectedViaKeys = false
					doConnect()
					continue
				}
				focus := win.GetFocus()
				otherBtn := false
				for _, bid := range []int{idSaveSession, idDeleteSession, idNewSession, idBrowse, idCancelConnect, idSFTPButton} {
					if focus == win.GetDlgItem(hwndSession, int32(bid)) {
						otherBtn = true
						break
					}
				}
				dropdownOpen := (focus == ctlAuth || focus == ctlProto) && win.SendMessage(focus, win.CB_GETDROPPEDSTATE, 0, 0) != 0
				if !otherBtn && !dropdownOpen && win.IsWindowEnabled(win.GetDlgItem(hwndSession, idConnect)) {
					doConnect()
					continue
				}
			}
			// Atas/Bawah di mana pun di jendela sesi (kecuali di daftar
			// itu sendiri yang sudah native, dan di ComboBox Auth yang
			// pakai panah untuk pilihannya sendiri) = ganti sesi terpilih.
			if msg.Message == win.WM_KEYDOWN && (msg.WParam == uintptr(win.VK_UP) || msg.WParam == uintptr(win.VK_DOWN)) {
				focus := win.GetFocus()
				if focus != ctlList && focus != ctlAuth && focus != ctlProto {
					if msg.WParam == uintptr(win.VK_UP) {
						moveSessionSelection(-1)
					} else {
						moveSessionSelection(1)
					}
					continue
				}
			}
			if win.IsDialogMessage(hwndSession, &msg) {
				continue
			}
		}
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
	}
}

