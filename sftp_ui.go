//go:build windows

// Jendela SFTP KERM (2026-10-30, atas permintaan user).
//
// Bentuk yang dipilih user (dari opsi yang ditawarkan): jendela SFTP
// TERPISAH, dua panel (kiri lokal, kanan remote), bukan tab di terminal
// dan bukan toolbar di atas terminal — supaya layout terminal yang
// sudah mapan tidak bergeser, dan file manager terlihat kedua sisinya
// sekaligus seperti WinSCP/FileZilla versi sederhana.
//
// Pemicu buka jendela (dua jalur, fungsi yang sama):
//   1. Di terminal: Ctrl+Shift+S (Ctrl+S polos tetap ke server, XOFF).
//   2. Di panel sesi: tombol "SFTP" di bawah status log, untuk sesi
//      yang sudah terhubung (kalau belum terhubung, status log memberi
//      tahu untuk Connect dulu).
// "SFTP here": remote path awal = direktori prompt terminal saat ini
// (Term.PromptDir, mis. /var/www atau ~), fallback ke home SFTP.
// Panel lokal awal = Downloads user.
//
// Semua operasi jaringan (buat client SFTP, refresh list, transfer)
// jalan di goroutine, hasil dikirim ke UI thread lewat WM_APP — sama
// seperti pola koneksi SSH yang sudah ada. Satu-satunya operasi SFTP
// di UI thread adalah Stat untuk cek timpa (satu round-trip, cepat
// dalam kondisi normal); transfer besar tidak pernah di UI thread,
// jadi jendela tidak freeze saat upload/download.
//
// v1: satu file sekali, direktori hanya untuk navigasi (bukan
// transfer rekursif). Tutup jendela SFTP tidak menutup terminal;
// tutup/putus terminal menutup jendela SFTP-nya.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"github.com/pkg/sftp"
	"golang.org/x/sys/windows"
)

const (
	sftpIdHeaderBanner = 3000 + iota
	sftpIdHeaderTitle
	sftpIdHeaderSub
	sftpIdLocalLabel
	sftpIdLocalPath
	sftpIdLocalUp
	sftpIdLocalRefresh
	sftpIdRemoteLabel
	sftpIdRemotePath
	sftpIdRemoteUp
	sftpIdRemoteRefresh
	sftpIdLocalList
	sftpIdRemoteList
	sftpIdUpload
	sftpIdDownload
	sftpIdCancel
	sftpIdProgress
	sftpIdProgressLabel
	sftpIdStatus
)

const (
	wmAppSFTPReady    = win.WM_APP + 10
	wmAppSFTPList     = win.WM_APP + 11
	wmAppSFTPProgress = win.WM_APP + 12
	wmAppSFTPDone     = win.WM_APP + 13
	wmAppSFTPDblClk   = win.WM_APP + 14
	wmAppSFTPOpDone   = win.WM_APP + 16
	wmAppSFTPDeleteKey = win.WM_APP + 17
)

const (
	sftpMenuRename    = 4000
	sftpMenuDelete    = 4001
	sftpMenuNewFolder = 4002
)

type sftpWin struct {
	hwnd     win.HWND
	term     *termWin
	client   *sftp.Client
	ready    bool
	closed   atomic.Bool
	localPath  string
	remotePath string
	localEntries  []SFTPEntry
	remoteEntries []SFTPEntry
	// Kontrol (diisi di WM_CREATE).
	ctlLocalPath, ctlRemotePath, ctlLocalList, ctlRemoteList win.HWND
	ctlUpload, ctlDownload, ctlCancel, ctlProgress, ctlProgressLabel, ctlStatus win.HWND
	localListOrig, remoteListOrig uintptr
	tabControls []win.HWND
	// Edit: file remote yang sedang dibuka di editor teks (per
	// remotePath, supaya Enter kedua pada file yang sama tidak
	// membuat temp kedua / watcher kedua). Dijaga editsMu karena
	// dibaca watcher goroutine dan ditulis UI thread.
	edits   map[string]*editSession
	editsMu sync.Mutex
	// Transfer.
	transferInProg bool
	cancel         atomic.Bool
	transferName   string
	transferDir    string // "upload" atau "download"
	progressSent   atomic.Int64
	progressTotal  atomic.Int64
	lastProgressPost time.Time
}

var (
	sftpWins     = map[win.HWND]*sftpWin{}
	sftpProc     = syscall.NewCallback(sftpWndProc)
	sftpListProc = syscall.NewCallback(sftpListWndProc)
	// pendingSFTPWin: jembatan WM_CREATE (lihat komentar di sana).
	pendingSFTPWin *sftpWin
	sftpReadyByID = map[uintptr]*sftpReadyResult{}
	sftpListByID  = map[uintptr]*sftpListResult{}
	sftpDoneByID  = map[uintptr]*sftpDoneResult{}
	sftpOpByID    = map[uintptr]*sftpOpResult{}
)

type sftpReadyResult struct {
	client *sftp.Client
	home   string
	err    error
}

type sftpListResult struct {
	side    string // "local" atau "remote"
	path    string
	entries []SFTPEntry
	err     error
}

type sftpDoneResult struct {
	name string
	dir  string
	err  error
}

type sftpOpResult struct {
	op   string // "rename", "delete", "newfolder"
	name string
	err  error
}

func storeSFTPReady(r *sftpReadyResult) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	sftpReadyByID[id] = r
	return id
}

func takeSFTPReady(id uintptr) *sftpReadyResult {
	msgMu.Lock()
	defer msgMu.Unlock()
	r := sftpReadyByID[id]
	delete(sftpReadyByID, id)
	return r
}

func storeSFTPList(r *sftpListResult) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	sftpListByID[id] = r
	return id
}

func takeSFTPList(id uintptr) *sftpListResult {
	msgMu.Lock()
	defer msgMu.Unlock()
	r := sftpListByID[id]
	delete(sftpListByID, id)
	return r
}

func storeSFTPOp(r *sftpOpResult) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	sftpOpByID[id] = r
	return id
}

func takeSFTPOp(id uintptr) *sftpOpResult {
	msgMu.Lock()
	defer msgMu.Unlock()
	r := sftpOpByID[id]
	delete(sftpOpByID, id)
	return r
}

func storeSFTPDone(r *sftpDoneResult) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	sftpDoneByID[id] = r
	return id
}

func takeSFTPDone(id uintptr) *sftpDoneResult {
	msgMu.Lock()
	defer msgMu.Unlock()
	r := sftpDoneByID[id]
	delete(sftpDoneByID, id)
	return r
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func sftpByHWND(hwnd win.HWND) *sftpWin { return sftpWins[hwnd] }

// sftpWindowForChild: jendela SFTP pemilik sebuah HWND (jendela itu
// sendiri atau salah satu kontrolnya). Dipakai Tab handler di message
// loop utama.
func sftpWindowForChild(hwnd win.HWND) *sftpWin {
	if w := sftpWins[hwnd]; w != nil {
		return w
	}
	parent := win.GetParent(hwnd)
	if parent != 0 {
		return sftpWins[parent]
	}
	return nil
}

// sftpHandlePathEnter: Enter di Edit path lokal / remote = menuju
// list directory path itu (mis. ketik D:\node lalu Enter, list lokal
// langsung pindah ke D:\node, tanpa klik Refresh). True kalau
// ditangani (message loop harus skip Dispatch, supaya Edit tidak beep).
// Berlaku untuk lokal DAN remote, supaya konsisten.
func sftpHandlePathEnter(msg *win.MSG) bool {
	if msg.Message != win.WM_KEYDOWN || msg.WParam != win.VK_RETURN {
		return false
	}
	w := sftpWindowForChild(msg.HWnd)
	if w == nil {
		return false
	}
	if msg.HWnd == w.ctlLocalPath && !w.transferInProg {
		w.refreshLocal(strings.TrimSpace(getText(w.ctlLocalPath)))
		return true
	}
	if msg.HWnd == w.ctlRemotePath && w.ready && !w.transferInProg {
		w.refreshRemote(strings.TrimSpace(getText(w.ctlRemotePath)))
		return true
	}
	return false
}

// sftpHandleTab: Tab / Shift+Tab di jendela SFTP pindah fokus ke
// kontrol berikutnya di tabControls. True kalau ditangani (message
// loop harus skip Dispatch). Tab manual, bukan IsDialogMessage,
// supaya Enter di path edit tidak salah jadi klik tombol default.
func sftpHandleTab(msg *win.MSG) bool {
	if msg.Message != win.WM_KEYDOWN || msg.WParam != win.VK_TAB {
		return false
	}
	w := sftpWindowForChild(msg.HWnd)
	if w == nil || len(w.tabControls) == 0 {
		return false
	}
	focus := win.GetFocus()
	idx := -1
	for i, h := range w.tabControls {
		if h == focus {
			idx = i
			break
		}
	}
	shift := win.GetKeyState(win.VK_SHIFT) < 0
	next := 0
	if idx < 0 {
		next = 0
	} else if shift {
		next = (idx - 1 + len(w.tabControls)) % len(w.tabControls)
	} else {
		next = (idx + 1) % len(w.tabControls)
	}
	win.SetFocus(w.tabControls[next])
	return true
}

func defaultLocalDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	dl := filepath.Join(home, "Downloads")
	if st, err := os.Stat(dl); err == nil && st.IsDir() {
		return dl
	}
	return home
}

// openSFTPForTerm: buka (atau tampilkan lagi) jendela SFTP untuk
// terminal t. Satu jendela SFTP per terminal.
func openSFTPForTerm(t *termWin) {
	if t == nil {
		return
	}
	if t.cfg.Protocol == "telnet" {
		msgBox(t.hwnd, "SFTP is only available for SSH sessions, not Telnet.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
		return
	}
	connMu.Lock()
	c := t.conn
	disconnected := t.disconnected
	connMu.Unlock()
	if c == nil || c.Client == nil || disconnected {
		msgBox(t.hwnd, "Terminal is not connected. Connect first, then open SFTP.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
		return
	}
	for _, w := range sftpWins {
		if w.term == t && !w.closed.Load() {
			win.ShowWindow(w.hwnd, win.SW_RESTORE)
			win.SetForegroundWindow(w.hwnd)
			win.SetFocus(w.hwnd)
			return
		}
	}
	w := &sftpWin{term: t, localPath: defaultLocalDir()}
	pendingSFTPWin = w
	hwnd := win.CreateWindowEx(0, utf16("SSHTermSFTP"),
		utf16("KERM Terminal - SFTP - "+termWho(t)),
		win.WS_OVERLAPPEDWINDOW, win.CW_USEDEFAULT, win.CW_USEDEFAULT, 1200, 720,
		0, 0, hInst, nil)
	pendingSFTPWin = nil
	if hwnd == 0 {
		msgBox(t.hwnd, "Failed to create SFTP window.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
		return
	}
	w.hwnd = hwnd
	sftpWins[hwnd] = w
	setWindowIcon(hwnd)
	win.ShowWindow(hwnd, win.SW_SHOW)
	win.UpdateWindow(hwnd)
}

// updateSFTPButton: tombol SFTP di panel sesi aktif hanya kalau ada
// terminal SSH terhubung (bukan Telnet, bukan putus). Dipilih DISABLE,
// bukan hide, supaya layout panel tidak loncat dan user tetap tahu
// tombol itu ada — tinggal Connect dulu. Dipanggil: saat panel dibuat,
// saat connect berhasil, dan saat terminal ditutup/putus.
func updateSFTPButton() {
	if hwndSession == 0 {
		return
	}
	btn := win.GetDlgItem(hwndSession, idSFTPButton)
	if btn == 0 {
		return
	}
	enabled := false
	for _, t := range termWins {
		connMu.Lock()
		c := t.conn
		disconnected := t.disconnected
		connMu.Unlock()
		if c != nil && c.Client != nil && !disconnected && t.cfg.Protocol != "telnet" {
			enabled = true
			break
		}
	}
	win.EnableWindow(btn, enabled)
}

// openSFTPFromPanel: tombol SFTP di panel sesi. Urutan terminal
// mengikuti termOrder (urutan dibuka), BUKAN iterasi map (acak).
// Kalau hanya ada satu terminal SSH terhubung, langsung buka itu
// (tidak nambah klik). Kalau banyak, tampilkan pemilih terminal —
// TIDAK lagi menebak dari form sesi (tebakan itu gagal untuk
// duplikat user@host yang sama dan untuk form yang tidak sedang
// terhubung; pemilih menampilkan nama sesi + user@host + direktori
// prompt, jadi duplikat yang sudah cd ke folder berbeda bisa
// dibedakan). Ctrl+Shift+S di terminal tetap jalur per-terminal
// yang tidak perlu memilih.
func openSFTPFromPanel() {
	var connected []*termWin
	for _, hwnd := range termOrder {
		t := termWins[hwnd]
		if t == nil {
			continue
		}
		connMu.Lock()
		c := t.conn
		disconnected := t.disconnected
		connMu.Unlock()
		if c != nil && c.Client != nil && !disconnected && t.cfg.Protocol != "telnet" {
			connected = append(connected, t)
		}
	}
	if len(connected) == 0 {
		appendStatus("SFTP: no connected SSH terminal. Connect first, then click SFTP (or Ctrl+Shift+S in the terminal).")
		return
	}
	if len(connected) == 1 {
		openSFTPForTerm(connected[0])
		return
	}
	openPickDialog(hwndSession, connected)
}

// closeSFTPForTerm: tutup semua jendela SFTP milik terminal t
// (dipanggil saat terminal ditutup / putus). Terminal tetap yang
// menutup, bukan sebaliknya.
func closeSFTPForTerm(t *termWin) {
	var toClose []*sftpWin
	for _, w := range sftpWins {
		if w.term == t {
			toClose = append(toClose, w)
		}
	}
	for _, w := range toClose {
		sftpClose(w)
		if w.hwnd != 0 {
			win.DestroyWindow(w.hwnd)
		}
	}
}

// sftpClose: hentikan transfer (kalau ada) + tutup client SFTP.
// Idempotent, aman dipanggil dari WM_CLOSE maupun WM_DESTROY.
// Peringatan HANYA kalau ada perubahan yang belum ter-upload
// (dirty / upload gagal). Dulu peringatan muncul untuk SEMUA sesi
// Edit, padahal sesi itu tetap ada walaupun editor sudah ditutup
// dan upload sudah finish — KERM tidak bisa tahu tab Notepad++
// sudah ditutup atau belum, jadi itu false alarm (laporan user
// 2026-10-30: Notepad++ sudah tutup, upload sudah finish, tapi
// peringatan tetap muncul). Sekarang kasus itu tutup diam-diam.
// File temp TIDAK dihapus saat tutup (supaya editan tersimpan tidak
// hilang dan tidak mengganggu editor yang mungkin masih terbuka).
func sftpClose(w *sftpWin) {
	if w == nil || w.closed.Load() {
		return
	}
	if temps := w.editPendingPaths(); len(temps) > 0 {
		msg := "There are " + fmt.Sprint(len(temps)) + " file(s) with changes not yet uploaded to the server. If you close this SFTP window now, those changes will NOT be uploaded.\n\nSave in the editor and wait for 'Upload finished' first, or keep a copy from:\n" + strings.Join(temps, "\n")
		msgBox(w.hwnd, msg, "SFTP", win.MB_ICONWARNING|win.MB_OK)
	}
	w.closed.Store(true)
	w.cancel.Store(true)
	if w.client != nil {
		_ = w.client.Close()
		w.client = nil
	}
}

func (w *sftpWin) appendLog(line string) {
	if w.ctlStatus == 0 {
		return
	}
	old := getText(w.ctlStatus)
	next := line
	if old != "" {
		next = old + "\r\n" + line
	}
	setText(w.ctlStatus, next)
	n := win.SendMessage(w.ctlStatus, win.WM_GETTEXTLENGTH, 0, 0)
	win.SendMessage(w.ctlStatus, win.EM_SETSEL, n, n)
	win.SendMessage(w.ctlStatus, win.EM_SCROLLCARET, 0, 0)
}

func (w *sftpWin) setTransferEnabled(enabled bool) {
	win.EnableWindow(w.ctlUpload, enabled)
	win.EnableWindow(w.ctlDownload, enabled)
	win.EnableWindow(w.ctlLocalList, enabled)
	win.EnableWindow(w.ctlRemoteList, enabled)
	win.EnableWindow(win.GetDlgItem(w.hwnd, sftpIdLocalUp), enabled)
	win.EnableWindow(win.GetDlgItem(w.hwnd, sftpIdLocalRefresh), enabled)
	win.EnableWindow(win.GetDlgItem(w.hwnd, sftpIdRemoteUp), enabled)
	win.EnableWindow(win.GetDlgItem(w.hwnd, sftpIdRemoteRefresh), enabled)
	win.EnableWindow(w.ctlCancel, !enabled)
}

func (w *sftpWin) refreshLocal(path string) {
	hwnd := w.hwnd
	go func() {
		entries, err := ListLocal(path)
		postMessage(hwnd, wmAppSFTPList, storeSFTPList(&sftpListResult{side: "local", path: path, entries: entries, err: err}), 0)
	}()
}

func (w *sftpWin) refreshRemote(path string) {
	hwnd := w.hwnd
	client := w.client
	go func() {
		if client == nil {
			postMessage(hwnd, wmAppSFTPList, storeSFTPList(&sftpListResult{side: "remote", path: path, err: errors.New("SFTP is not connected")}), 0)
			return
		}
		entries, err := ListRemote(client, path)
		postMessage(hwnd, wmAppSFTPList, storeSFTPList(&sftpListResult{side: "remote", path: path, entries: entries, err: err}), 0)
	}()
}

// Kolom ListView gaya WinSCP: Name | Size | Type | Changed.
var sftpColumns = []string{"Name", "Size", "Type", "Changed"}

func sftpSetupListView(list win.HWND) {
	win.SendMessage(list, win.LVM_SETEXTENDEDLISTVIEWSTYLE,
		uintptr(win.LVS_EX_FULLROWSELECT|win.LVS_EX_GRIDLINES),
		uintptr(win.LVS_EX_FULLROWSELECT|win.LVS_EX_GRIDLINES))
	for i, title := range sftpColumns {
		text := utf16(title)
		col := win.LVCOLUMN{
			Mask:     win.LVCF_FMT | win.LVCF_WIDTH | win.LVCF_TEXT | win.LVCF_SUBITEM,
			Fmt:      win.LVCFMT_LEFT,
			Cx:       120,
			PszText:  text,
			ISubItem: int32(i),
		}
		if i == 1 {
			col.Fmt = win.LVCFMT_RIGHT
		}
		win.SendMessage(list, win.LVM_INSERTCOLUMN, uintptr(i), uintptr(unsafe.Pointer(&col)))
	}
}

func sftpSetItemText(list win.HWND, item, sub int32, text string) {
	t := utf16(text)
	it := win.LVITEM{Mask: win.LVIF_TEXT, IItem: item, ISubItem: sub, PszText: t}
	win.SendMessage(list, win.LVM_SETITEMTEXT, uintptr(item), uintptr(unsafe.Pointer(&it)))
}

func (w *sftpWin) fillList(list win.HWND, entries []SFTPEntry) {
	win.SendMessage(list, win.WM_SETREDRAW, 0, 0)
	win.SendMessage(list, win.LVM_DELETEALLITEMS, 0, 0)
	for i, e := range entries {
		name := utf16(e.Name)
		it := win.LVITEM{Mask: win.LVIF_TEXT, IItem: int32(i), ISubItem: 0, PszText: name}
		win.SendMessage(list, win.LVM_INSERTITEM, 0, uintptr(unsafe.Pointer(&it)))
		sftpSetItemText(list, int32(i), 1, entrySizeText(e))
		sftpSetItemText(list, int32(i), 2, entryType(e))
		sftpSetItemText(list, int32(i), 3, entryChangedText(e))
	}
	win.SendMessage(list, win.WM_SETREDRAW, 1, 0)
	win.InvalidateRect(list, nil, true)
}

func listSelection(list win.HWND, entries []SFTPEntry) *SFTPEntry {
	idx := int(win.SendMessage(list, win.LVM_GETNEXTITEM, ^uintptr(0), uintptr(win.LVNI_SELECTED)))
	if idx < 0 || idx >= len(entries) {
		return nil
	}
	e := entries[idx]
	return &e
}

func (w *sftpWin) localSelection() *SFTPEntry {
	return listSelection(w.ctlLocalList, w.localEntries)
}

func (w *sftpWin) remoteSelection() *SFTPEntry {
	return listSelection(w.ctlRemoteList, w.remoteEntries)
}

// layoutSFTP: tata ulang semua kontrol SFTP mengikuti ukuran klien
// jendela (dipanggil dari WM_CREATE dan WM_SIZE). Ini yang membuat
// jendela SFTP terasa seperti WinSCP: di-maximize, panel ikut
// membesar, bukan tertinggal kecil di pojok kiri atas.
func layoutSFTP(w *sftpWin) {
	if w == nil || w.hwnd == 0 || w.ctlLocalList == 0 {
		return
	}
	var rc win.RECT
	win.GetClientRect(w.hwnd, &rc)
	cw := int32(rc.Right - rc.Left)
	ch := int32(rc.Bottom - rc.Top)
	if cw < 600 || ch < 400 {
		return
	}
	// Header full width.
	win.MoveWindow(win.GetDlgItem(w.hwnd, sftpIdHeaderBanner), 0, 0, cw, 64, true)
	win.MoveWindow(win.GetDlgItem(w.hwnd, sftpIdHeaderTitle), 16, 8, cw-32, 30, true)
	win.MoveWindow(win.GetDlgItem(w.hwnd, sftpIdHeaderSub), 16, 36, cw-32, 20, true)
	// Dua panel: kiri + celah tengah 128 + kanan, margin 16.
	// Celah 128 (dulu 80) supaya tombol "Upload >>" / "<< Download"
	// tidak terpotong di font tebal -16 ("Ipload >", "Downlo").
	panelW := (cw - 32 - 128) / 2
	leftX := int32(16)
	rightX := leftX + panelW + 128
	// Baris path (y=72): edit + Up (50) + Refresh (64), per panel.
	for _, p := range []struct {
		edit, up, refresh win.HWND
		x                  int32
	}{
		{w.ctlLocalPath, win.GetDlgItem(w.hwnd, sftpIdLocalUp), win.GetDlgItem(w.hwnd, sftpIdLocalRefresh), leftX},
		{w.ctlRemotePath, win.GetDlgItem(w.hwnd, sftpIdRemoteUp), win.GetDlgItem(w.hwnd, sftpIdRemoteRefresh), rightX},
	} {
		editW := panelW - 50 - 64 - 16
		if editW < 100 {
			editW = 100
		}
		win.MoveWindow(p.edit, p.x, 72, editW, 22, true)
		win.MoveWindow(p.up, p.x+editW+8, 72, 50, 24, true)
		win.MoveWindow(p.refresh, p.x+editW+66, 72, 64, 24, true)
	}
	// Daftar: y=104 sampai atas area progress (ch-140).
	listTop := int32(104)
	listH := ch - listTop - 140
	if listH < 100 {
		listH = 100
	}
	win.MoveWindow(w.ctlLocalList, leftX, listTop, panelW, listH, true)
	win.MoveWindow(w.ctlRemoteList, rightX, listTop, panelW, listH, true)
	// Tombol tengah, vertikal di tengah daftar. Lebar 120 di celah
	// 128, supaya teks tebal tidak terpotong. Hint tengah yang
	// dulu lebarnya 72 (juga terpotong: "Double-cli") DIHAPUS —
	// petunjuk double-click pindah ke subtitle header yang lebar.
	midX := leftX + panelW + 4
	midY := listTop + listH/2 - 32
	win.MoveWindow(w.ctlUpload, midX, midY, 120, 28, true)
	win.MoveWindow(w.ctlDownload, midX, midY+36, 120, 28, true)
	// Area bawah, menempel ke bawah jendela.
	win.MoveWindow(w.ctlProgress, 16, ch-140, cw-32-88, 22, true)
	win.MoveWindow(w.ctlCancel, cw-16-80, ch-140, 80, 24, true)
	win.MoveWindow(w.ctlProgressLabel, 16, ch-114, cw-32, 20, true)
	win.MoveWindow(w.ctlStatus, 16, ch-90, cw-32, 74, true)
	// Lebar kolom: Name fleksibel, sisanya tetap (seperti WinSCP).
	nameW := panelW - 90 - 90 - 124
	if nameW < 120 {
		nameW = 120
	}
	for _, list := range []win.HWND{w.ctlLocalList, w.ctlRemoteList} {
		win.SendMessage(list, win.LVM_SETCOLUMNWIDTH, 0, uintptr(nameW))
		win.SendMessage(list, win.LVM_SETCOLUMNWIDTH, 1, 90)
		win.SendMessage(list, win.LVM_SETCOLUMNWIDTH, 2, 90)
		win.SendMessage(list, win.LVM_SETCOLUMNWIDTH, 3, 124)
	}
}

// browseLocalFile: dialog pilih file (filter semua file), mulai dari
// direktori lokal saat ini. Dipakai Upload kalau tidak ada seleksi.
func browseLocalFile(owner win.HWND, startDir string) string {
	filter, _ := windows.UTF16FromString("All files (*.*)\x00*.*\x00")
	filter = append(filter, 0)
	fileBuf := make([]uint16, 260)
	ofn := win.OPENFILENAME{
		LStructSize:  uint32(unsafe.Sizeof(win.OPENFILENAME{})),
		HwndOwner:    owner,
		HInstance:    hInst,
		LpstrFilter:  &filter[0],
		LpstrFile:    &fileBuf[0],
		NMaxFile:     260,
		LpstrInitialDir: utf16(startDir),
		Flags:        0x00080000 | 0x00001000, // OFN_FILEMUSTEXIST | OFN_PATHMUSTEXIST
	}
	if win.GetOpenFileName(&ofn) {
		return windows.UTF16ToString(fileBuf)
	}
	return ""
}

// askOverwrite: dialog timpa dengan detail ukuran + tanggal kedua
// sisi. True = timpa, false = lewati/batal. Dipanggil dari UI thread.
func askOverwrite(owner win.HWND, name, target string, existingSize int64, existingTime time.Time, newSize int64, newTime time.Time) bool {
	text := fmt.Sprintf("File already exists:\n%s\n\nExisting: %s, %s\nNew: %s, %s\n\nOverwrite?",
		target, formatSize(existingSize), existingTime.Format("2006-01-02 15:04"),
		formatSize(newSize), newTime.Format("2006-01-02 15:04"))
	return msgBox(owner, text, "Overwrite?", win.MB_YESNO|win.MB_ICONQUESTION) == win.IDYES
}

func (w *sftpWin) startUpload() {
	if !w.ready || w.client == nil || w.transferInProg {
		return
	}
	var localPath, name string
	if sel := w.localSelection(); sel != nil {
		if sel.IsParent {
			msgBox(w.hwnd, "Parent directory (..) cannot be transferred. Double-click it to go up.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
			return
		}
		if sel.IsDir {
			msgBox(w.hwnd, "Directories are not supported in SFTP v1. Please select a file.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
			return
		}
		localPath = localJoin(w.localPath, sel.Name)
		name = sel.Name
	} else {
		picked := browseLocalFile(w.hwnd, w.localPath)
		if picked == "" {
			return
		}
		localPath = picked
		name = filepath.Base(picked)
	}
	remotePath := remoteJoin(w.remotePath, name)
	info, err := os.Stat(localPath)
	if err != nil {
		w.appendLog("Upload failed: " + err.Error())
		return
	}
	overwrite := true
	if existing, err := w.client.Stat(remotePath); err == nil {
		overwrite = askOverwrite(w.hwnd, name, remotePath, existing.Size(), existing.ModTime(), info.Size(), info.ModTime())
		if !overwrite {
			w.appendLog("Skipped: " + name)
			return
		}
	}
	w.startTransfer("upload", name, localPath, remotePath, overwrite)
}

func (w *sftpWin) startDownload() {
	if !w.ready || w.client == nil || w.transferInProg {
		return
	}
	sel := w.remoteSelection()
	if sel == nil {
		msgBox(w.hwnd, "Please select a file in the remote panel first.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
		return
	}
	if sel.IsParent {
		msgBox(w.hwnd, "Parent directory (..) cannot be transferred. Double-click it to go up.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
		return
	}
	if sel.IsDir {
		msgBox(w.hwnd, "Directories are not supported in SFTP v1. Please select a file.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
		return
	}
	remotePath := remoteJoin(w.remotePath, sel.Name)
	localPath := localJoin(w.localPath, sel.Name)
	overwrite := true
	if existing, err := os.Stat(localPath); err == nil {
		overwrite = askOverwrite(w.hwnd, sel.Name, localPath, existing.Size(), existing.ModTime(), sel.Size, sel.ModTime)
		if !overwrite {
			w.appendLog("Skipped: " + sel.Name)
			return
		}
	}
	w.startTransfer("download", sel.Name, localPath, remotePath, overwrite)
}

func (w *sftpWin) startTransfer(dir, name, localPath, remotePath string, overwrite bool) {
	w.transferInProg = true
	w.transferDir = dir
	w.transferName = name
	w.cancel.Store(false)
	w.progressSent.Store(0)
	w.progressTotal.Store(0)
	w.lastProgressPost = time.Time{}
	w.setTransferEnabled(false)
	if w.ctlProgress != 0 {
		win.SendMessage(w.ctlProgress, win.PBM_SETPOS, 0, 0)
	}
	setText(w.ctlProgressLabel, dir+"ing "+name+" ...")
	w.appendLog(capFirst(dir) + "ing: " + name)
	hwnd := w.hwnd
	client := w.client
	cancel := &w.cancel
	onProgress := func(sent, total int64) {
		w.progressSent.Store(sent)
		w.progressTotal.Store(total)
		now := time.Now()
		if sent == total || now.Sub(w.lastProgressPost) >= 100*time.Millisecond {
			w.lastProgressPost = now
			postMessage(hwnd, wmAppSFTPProgress, uintptr(sent), uintptr(total))
		}
	}
	go func() {
		var err error
		if dir == "upload" {
			err = UploadFile(client, localPath, remotePath, overwrite, cancel, onProgress)
		} else {
			err = DownloadFile(client, remotePath, localPath, overwrite, cancel, onProgress)
		}
		postMessage(hwnd, wmAppSFTPDone, storeSFTPDone(&sftpDoneResult{name: name, dir: dir, err: err}), 0)
	}()
}

// sftpRunOp: jalankan operasi remote (rename/delete/newfolder) di
// goroutine, hasil ke UI thread lewat WM_APP. UI tidak freeze, dan
// list remote di-refresh kalau berhasil.
func (w *sftpWin) sftpRunOp(op, name string, fn func(client *sftp.Client) error) {
	hwnd := w.hwnd
	client := w.client
	go func() {
		var err error
		if client == nil {
			err = errors.New("SFTP is not connected")
		} else {
			err = fn(client)
		}
		postMessage(hwnd, wmAppSFTPOpDone, storeSFTPOp(&sftpOpResult{op: op, name: name, err: err}), 0)
	}()
}

// sftpDeleteSelected: hapus file/folder remote yang terpilih, dengan
// konfirmasi detail (nama, ukuran, tanggal; folder harus kosong).
// Dipakai bersama oleh klik kanan Delete DAN tombol Delete keyboard,
// supaya perilakunya identik. ".." dan tanpa seleksi: tidak ada popup,
// hanya log (untuk keyboard) atau tidak terjadi apa-apa (untuk menu,
// item Delete memang tidak ditampilkan untuk "..").
func (w *sftpWin) sftpDeleteSelected() {
	if !w.ready || w.client == nil || w.transferInProg {
		return
	}
	sel := w.remoteSelection()
	if sel == nil || sel.IsParent {
		return
	}
	target := remoteJoin(w.remotePath, sel.Name)
	var detail string
	if sel.IsDir {
		detail = "Delete this folder?\n" + target + "\n\nFolder must be empty.\nThis cannot be undone."
	} else {
		detail = "Delete this file?\n" + target + "\nSize: " + entrySizeText(*sel) + ", Changed: " + entryChangedText(*sel) + "\n\nThis cannot be undone."
	}
	if msgBox(w.hwnd, detail, "Delete", win.MB_YESNO|win.MB_ICONWARNING) != win.IDYES {
		w.appendLog("Delete cancelled: " + sel.Name)
		return
	}
	w.appendLog("Deleting: " + sel.Name)
	w.sftpRunOp("delete", sel.Name, func(client *sftp.Client) error {
		if sel.IsDir {
			return client.RemoveDirectory(target)
		}
		return client.Remove(target)
	})
}

// sftpShowRemoteMenu: klik kanan di panel remote. Kalau klik di atas
// item, item itu dipilih dulu (ListView tidak otomatis memilih saat
// klik kanan), lalu menu Rename / Delete / New folder muncul. Kalau
// klik di area kosong, hanya New folder yang ditawarkan. ".." bukan
// file nyata: klik kanan ".." hanya New folder.
func (w *sftpWin) sftpShowRemoteMenu(screenX, screenY int32) {
	if !w.ready || w.client == nil || w.transferInProg {
		return
	}
	pt := win.POINT{X: screenX, Y: screenY}
	if !win.ScreenToClient(w.ctlRemoteList, &pt) {
		return
	}
	hit := win.LVHITTESTINFO{Pt: pt}
	win.SendMessage(w.ctlRemoteList, win.LVM_HITTEST, 0, uintptr(unsafe.Pointer(&hit)))
	selIdx := -1
	if hit.IItem >= 0 {
		selIdx = int(hit.IItem)
		// Pilih item yang diklik kanan.
		it := win.LVITEM{State: win.LVIS_SELECTED, StateMask: win.LVIS_SELECTED}
		win.SendMessage(w.ctlRemoteList, win.LVM_SETITEMSTATE, uintptr(hit.IItem), uintptr(unsafe.Pointer(&it)))
	}
	var sel *SFTPEntry
	if selIdx >= 0 && selIdx < len(w.remoteEntries) {
		e := w.remoteEntries[selIdx]
		sel = &e
	}
	menu := win.CreatePopupMenu()
	if menu == 0 {
		return
	}
	defer win.DestroyMenu(menu)
	pos := uint32(0)
	addMenuItem := func(id uint32, text string) {
		t := utf16(text)
		mii := win.MENUITEMINFO{
			CbSize:     uint32(unsafe.Sizeof(win.MENUITEMINFO{})),
			FMask:      win.MIIM_STRING | win.MIIM_ID,
			FType:      win.MFT_STRING,
			WID:        id,
			DwTypeData: t,
			Cch:        uint32(len(text)),
		}
		win.InsertMenuItem(menu, pos, true, &mii)
		pos++
	}
	canEdit := sel != nil && !sel.IsParent
	if canEdit {
		addMenuItem(sftpMenuRename, "Rename")
		addMenuItem(sftpMenuDelete, "Delete")
	}
	addMenuItem(sftpMenuNewFolder, "New folder")
	cmd := win.TrackPopupMenu(menu, win.TPM_RETURNCMD|win.TPM_RIGHTBUTTON, screenX, screenY, 0, w.hwnd, nil)
	switch cmd {
	case sftpMenuRename:
		if sel == nil {
			return
		}
		oldPath := remoteJoin(w.remotePath, sel.Name)
		openInputDialog(w.hwnd, "Rename", "New name:", sel.Name, "rename", oldPath)
	case sftpMenuDelete:
		w.sftpDeleteSelected()
	case sftpMenuNewFolder:
		openInputDialog(w.hwnd, "New folder", "Folder name:", "", "newfolder", w.remotePath)
	}
}

// sftpListWndProc: subclass ListView SFTP, hanya untuk menangkap
// double-click. Alasan subclass, bukan WM_NOTIFY: WM_NOTIFY membawa
// pointer NMHDR di lParam, dan konversi uintptr->unsafe.Pointer itu
// diprotes go vet (possible misuse). Subclass ini vet-clean: pesan
// double-click adalah WM_LBUTTONDBLCLK (lParam = koordinat, bukan
// pointer), dan pemberitahuan ke parent lewat PostMessage dengan
// HWND sebagai wParam (handle, bukan pointer Go).
func sftpListWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	parent := win.GetParent(hwnd)
	w := sftpWins[parent]
	var orig uintptr
	if w != nil {
		if hwnd == w.ctlLocalList {
			orig = w.localListOrig
		} else if hwnd == w.ctlRemoteList {
			orig = w.remoteListOrig
		}
	}
	if orig == 0 {
		return win.DefWindowProc(hwnd, msg, wParam, lParam)
	}
	res := win.CallWindowProc(orig, hwnd, msg, wParam, lParam)
	if w != nil && (msg == win.WM_LBUTTONDBLCLK ||
		// Enter di list (setelah pilih pakai Up/Down) = sama seperti
		// double-click: folder = masuk, file = transfer, ".." = kembali.
		(msg == win.WM_KEYDOWN && wParam == win.VK_RETURN)) {
		postMessage(parent, wmAppSFTPDblClk, uintptr(hwnd), 0)
	}
	// Tombol Delete keyboard: HANYA panel remote (server), bukan
	// panel lokal (file lokal tidak dihapus dari KERM). Delete di
	// Edit path tetap menghapus karakter, karena Edit bukan ListView
	// dan tidak lewat subclass ini.
	if w != nil && hwnd == w.ctlRemoteList &&
		msg == win.WM_KEYDOWN && wParam == win.VK_DELETE {
		postMessage(parent, wmAppSFTPDeleteKey, 0, 0)
	}
	return res
}

func sftpWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	w := sftpByHWND(hwnd)
	switch msg {
	case win.WM_CTLCOLORSTATIC:
		hdc := win.HDC(wParam)
		ctl := win.HWND(lParam)
		if w != nil && (ctl == win.GetDlgItem(hwnd, sftpIdHeaderBanner) || ctl == win.GetDlgItem(hwnd, sftpIdHeaderTitle) || ctl == win.GetDlgItem(hwnd, sftpIdHeaderSub)) {
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
		// BUG 2026-10-30 (panel SFTP blank putih): WM_CREATE jalan
		// DI DALAM CreateWindowEx, SEBELUM CreateWindowEx kembali
		// dan SEBELUM openSFTPForTerm sempat mengisi sftpWins.
		// Jadi sftpByHWND(hwnd) di sini selalu nil, handler lama
		// return 0 lebih awal, dan TIDAK ADA kontrol yang dibuat —
		// jendela tampil kosong. Perbaikan: openSFTPForTerm
		// menaruh sftpWin di pendingSFTPWin SEBELUM CreateWindowEx,
		// handler ini mengambilnya di sini dan mengisi map + hwnd
		// di sini juga. Aman karena CreateWindowEx sinkron di UI
		// thread: WM_CREATE selalu jalan sebelum CreateWindowEx
		// kembali, jadi tidak ada dua jendela yang berebut pending.
		if w == nil {
			w = pendingSFTPWin
			if w != nil {
				w.hwnd = hwnd
				sftpWins[hwnd] = w
			}
		}
		if w == nil {
			return 0
		}
		// Header banner gaya KERM. Posisi awal untuk 1200x720;
		// layoutSFTP di akhir WM_CREATE + WM_SIZE yang mengatur
		// ulang semuanya mengikuti ukuran jendela sebenarnya.
		createControl("STATIC", "", 0, 0, 0, 1200, 64, hwnd, sftpIdHeaderBanner)
		createControl("STATIC", "KERM Terminal - SFTP", 0, 16, 8, 400, 30, hwnd, sftpIdHeaderTitle)
		createControl("STATIC", "Local (left)  |  Remote (right)  —  Double-click / Enter: folder = enter, local file = upload, remote text file = edit (save uploads back), other remote file = download.", 0, 16, 36, 1168, 20, hwnd, sftpIdHeaderSub)
		// Baris path per panel (gaya WinSCP: path di atas daftarnya
		// masing-masing, bukan dua baris full-width). Posisi awal
		// di sini hanya placeholder, layoutSFTP yang menata final.
		w.ctlLocalPath = createControl("EDIT", w.localPath, win.WS_BORDER|win.ES_AUTOHSCROLL, 16, 72, 414, 22, hwnd, sftpIdLocalPath)
		createControl("BUTTON", "Up", 0, 438, 72, 50, 24, hwnd, sftpIdLocalUp)
		createControl("BUTTON", "Refresh", 0, 496, 72, 64, 24, hwnd, sftpIdLocalRefresh)
		w.ctlRemotePath = createControl("EDIT", "Connecting SFTP...", win.WS_BORDER|win.ES_AUTOHSCROLL, 640, 72, 414, 22, hwnd, sftpIdRemotePath)
		createControl("BUTTON", "Up", 0, 1062, 72, 50, 24, hwnd, sftpIdRemoteUp)
		createControl("BUTTON", "Refresh", 0, 1120, 72, 64, 24, hwnd, sftpIdRemoteRefresh)
		// Dua ListView kolom (gaya WinSCP) + tombol tengah.
		w.ctlLocalList = createControl("SysListView32", "", win.WS_BORDER|win.WS_VSCROLL|win.LVS_REPORT|win.LVS_SINGLESEL|win.LVS_SHOWSELALWAYS, 16, 104, 544, 476, hwnd, sftpIdLocalList)
		w.ctlRemoteList = createControl("SysListView32", "", win.WS_BORDER|win.WS_VSCROLL|win.LVS_REPORT|win.LVS_SINGLESEL|win.LVS_SHOWSELALWAYS, 640, 104, 544, 476, hwnd, sftpIdRemoteList)
		sftpSetupListView(w.ctlLocalList)
		sftpSetupListView(w.ctlRemoteList)
		w.localListOrig = win.SetWindowLongPtr(w.ctlLocalList, win.GWLP_WNDPROC, sftpListProc)
		w.remoteListOrig = win.SetWindowLongPtr(w.ctlRemoteList, win.GWLP_WNDPROC, sftpListProc)
		w.ctlUpload = createControl("BUTTON", "Upload >>", 0, 564, 280, 120, 28, hwnd, sftpIdUpload)
		w.ctlDownload = createControl("BUTTON", "<< Download", 0, 564, 316, 120, 28, hwnd, sftpIdDownload)
		// Progress + label + Cancel + status log (menempel bawah).
		w.ctlProgress = createControl("msctls_progress32", "", win.PBS_SMOOTH, 16, 580, 1080, 22, hwnd, sftpIdProgress)
		if w.ctlProgress != 0 {
			st := win.GetWindowLong(w.ctlProgress, win.GWL_STYLE)
			win.SetWindowLong(w.ctlProgress, win.GWL_STYLE, st &^ int32(win.WS_TABSTOP))
			win.SendMessage(w.ctlProgress, win.PBM_SETRANGE32, 0, 100)
		}
		w.ctlCancel = createControl("BUTTON", "Cancel", 0, 1104, 580, 80, 24, hwnd, sftpIdCancel)
		win.EnableWindow(w.ctlCancel, false)
		w.ctlProgressLabel = createControl("STATIC", "Ready.", 0, 16, 606, 1168, 20, hwnd, sftpIdProgressLabel)
		w.ctlStatus = createControl("EDIT", "", win.WS_BORDER|win.ES_MULTILINE|win.ES_READONLY|win.WS_VSCROLL, 16, 630, 1168, 74, hwnd, sftpIdStatus)
		if w.ctlStatus != 0 {
			st := win.GetWindowLong(w.ctlStatus, win.GWL_STYLE)
			win.SetWindowLong(w.ctlStatus, win.GWL_STYLE, st &^ int32(win.WS_TABSTOP))
		}
		// Tab order = urutan pembuatan kontrol interaktif.
		w.tabControls = []win.HWND{
			w.ctlLocalPath, win.GetDlgItem(hwnd, sftpIdLocalUp), win.GetDlgItem(hwnd, sftpIdLocalRefresh),
			w.ctlRemotePath, win.GetDlgItem(hwnd, sftpIdRemoteUp), win.GetDlgItem(hwnd, sftpIdRemoteRefresh),
			w.ctlLocalList, w.ctlRemoteList, w.ctlUpload, w.ctlDownload, w.ctlCancel,
		}
		// Font Segoe UI untuk semua kontrol anak.
		cb := syscall.NewCallback(func(child win.HWND, _ uintptr) uintptr {
			win.SendMessage(child, win.WM_SETFONT, uintptr(uiFont), 1)
			return 1
		})
		fontEnumCallback = cb
		win.EnumChildWindows(hwnd, cb, 0)
		setCtrlFont(win.GetDlgItem(hwnd, sftpIdHeaderTitle), headerFont)
		setCtrlFont(win.GetDlgItem(hwnd, sftpIdHeaderSub), headerSubFont)
		setCtrlFont(w.ctlUpload, uiFontBold)
		setCtrlFont(w.ctlDownload, uiFontBold)
		layoutSFTP(w)
		w.appendLog("Connecting SFTP...")
		// Buat client SFTP di goroutine (subsystem + INIT bisa lambat).
		term := w.term
		hwndCopy := hwnd
		go func() {
			connMu.Lock()
			c := term.conn
			disconnected := term.disconnected
			connMu.Unlock()
			if c == nil || c.Client == nil || disconnected {
				if !w.closed.Load() {
					postMessage(hwndCopy, wmAppSFTPReady, storeSFTPReady(&sftpReadyResult{err: errors.New("terminal is not connected")}), 0)
				}
				return
			}
			client, err := sftp.NewClient(c.Client)
			if err != nil {
				if !w.closed.Load() {
					postMessage(hwndCopy, wmAppSFTPReady, storeSFTPReady(&sftpReadyResult{err: err}), 0)
				}
				return
			}
			// Jendela ditutup selagi client dibuat: tutup client
			// ini juga, jangan diposting ke jendela yang sudah mati.
			if w.closed.Load() {
				_ = client.Close()
				return
			}
			home, err := client.Getwd()
			if err != nil {
				home = ""
			}
			if w.closed.Load() {
				_ = client.Close()
				return
			}
			postMessage(hwndCopy, wmAppSFTPReady, storeSFTPReady(&sftpReadyResult{client: client, home: home}), 0)
		}()
		return 0
	case wmAppSFTPReady:
		if w == nil {
			return 0
		}
		res := takeSFTPReady(wParam)
		if res == nil {
			return 0
		}
		if res.err != nil {
			w.appendLog("SFTP failed: " + res.err.Error())
			msgBox(hwnd, "SFTP failed: "+res.err.Error()+"\n\nThe server may not have an sftp-server subsystem.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
			win.DestroyWindow(hwnd)
			return 0
		}
		w.client = res.client
		w.ready = true
		// SFTP here: prompt dir terminal, fallback home.
		promptDir := ""
		if w.term != nil && w.term.term != nil {
			promptDir = w.term.term.PromptDir()
		}
		w.remotePath = resolvePromptDir(promptDir, res.home)
		if w.remotePath == "" {
			w.remotePath = "."
		}
		setText(w.ctlRemotePath, w.remotePath)
		w.appendLog("SFTP connected. Remote: " + w.remotePath)
		w.refreshLocal(w.localPath)
		w.refreshRemote(w.remotePath)
		return 0
	case wmAppSFTPList:
		if w == nil {
			return 0
		}
		res := takeSFTPList(wParam)
		if res == nil {
			return 0
		}
		if res.err != nil {
			w.appendLog("List failed (" + res.side + "): " + res.err.Error())
			return 0
		}
		// ".." selalu di paling atas, lokal + remote.
		entries := withParentEntry(res.entries)
		if res.side == "local" {
			w.localPath = res.path
			w.localEntries = entries
			setText(w.ctlLocalPath, res.path)
			w.fillList(w.ctlLocalList, entries)
		} else {
			w.remotePath = res.path
			w.remoteEntries = entries
			setText(w.ctlRemotePath, res.path)
			w.fillList(w.ctlRemoteList, entries)
		}
		return 0
	case wmAppSFTPProgress:
		if w == nil {
			return 0
		}
		sent := int64(wParam)
		total := int64(lParam)
		pct := 0
		if total > 0 {
			pct = int(sent * 100 / total)
			if pct > 100 {
				pct = 100
			}
		}
		if w.ctlProgress != 0 {
			win.SendMessage(w.ctlProgress, win.PBM_SETPOS, uintptr(pct), 0)
		}
		setText(w.ctlProgressLabel, fmt.Sprintf("%sing %s ... %d%% (%s / %s)", w.transferDir, w.transferName, pct, formatSize(sent), formatSize(total)))
		return 0
	case wmAppSFTPDone:
		if w == nil {
			return 0
		}
		res := takeSFTPDone(wParam)
		if res == nil {
			return 0
		}
		w.transferInProg = false
		w.setTransferEnabled(true)
		if w.ctlProgress != 0 {
			if res.err == nil {
				win.SendMessage(w.ctlProgress, win.PBM_SETPOS, 100, 0)
			} else {
				win.SendMessage(w.ctlProgress, win.PBM_SETPOS, 0, 0)
			}
		}
		if res.err == nil {
			setText(w.ctlProgressLabel, capFirst(res.dir)+" finished: "+res.name)
			w.appendLog(capFirst(res.dir) + " finished: " + res.name)
		} else if errors.Is(res.err, ErrCancelled) {
			setText(w.ctlProgressLabel, "Cancelled: "+res.name)
			w.appendLog("Cancelled: " + res.name + " (partial file removed)")
		} else {
			setText(w.ctlProgressLabel, "Failed: "+res.name)
			w.appendLog("Failed: " + res.name + ": " + res.err.Error())
		}
		// Daftar diperbarui supaya file baru / perubahan terlihat.
		w.refreshLocal(w.localPath)
		if w.client != nil {
			w.refreshRemote(w.remotePath)
		}
		return 0
	case win.WM_COMMAND:
		if w == nil {
			return 0
		}
		id := int32(wParam & 0xFFFF)
		code := int32(wParam >> 16)
		switch id {
		case sftpIdLocalUp:
			if code == win.BN_CLICKED && !w.transferInProg {
				w.refreshLocal(localParent(strings.TrimSpace(getText(w.ctlLocalPath))))
			}
		case sftpIdLocalRefresh:
			if code == win.BN_CLICKED && !w.transferInProg {
				w.refreshLocal(strings.TrimSpace(getText(w.ctlLocalPath)))
			}
		case sftpIdRemoteUp:
			if code == win.BN_CLICKED && w.ready && !w.transferInProg {
				w.refreshRemote(remoteParent(strings.TrimSpace(getText(w.ctlRemotePath))))
			}
		case sftpIdRemoteRefresh:
			if code == win.BN_CLICKED && w.ready && !w.transferInProg {
				w.refreshRemote(strings.TrimSpace(getText(w.ctlRemotePath)))
			}
		case sftpIdUpload:
			if code == win.BN_CLICKED {
				w.startUpload()
			}
		case sftpIdDownload:
			if code == win.BN_CLICKED {
				w.startDownload()
			}
		case sftpIdCancel:
			if code == win.BN_CLICKED && w.transferInProg {
				w.cancel.Store(true)
				win.EnableWindow(w.ctlCancel, false)
				w.appendLog("Cancelling...")
			}
		}
		return 0
	case win.WM_SIZE:
		// Resize / maximize: panel ikut membesar (gaya WinSCP).
		// wParam SIZE_MINIMIZED dilewati supaya layout tidak
		// hancur saat minimize (sama seperti terminal).
		if w != nil && !shouldSkipTermResize(wParam) {
			layoutSFTP(w)
		}
		return 0
	case wmAppSFTPDeleteKey:
		// Tombol Delete keyboard di panel remote = fungsi delete
		// yang sama dengan klik kanan Delete (termasuk konfirmasi).
		if w != nil {
			w.sftpDeleteSelected()
		}
		return 0
	case wmAppSFTPDblClk:
		// Double-click dari subclass ListView (wParam = HWND list).
		if w != nil {
			if win.HWND(wParam) == w.ctlLocalList && !w.transferInProg {
				if sel := w.localSelection(); sel != nil {
					if sel.IsParent {
						w.refreshLocal(localParent(strings.TrimSpace(getText(w.ctlLocalPath))))
					} else if sel.IsDir {
						w.refreshLocal(localJoin(w.localPath, sel.Name))
					} else {
						w.startUpload()
					}
				}
			} else if win.HWND(wParam) == w.ctlRemoteList && w.ready && !w.transferInProg {
				if sel := w.remoteSelection(); sel != nil {
					if sel.IsParent {
						w.refreshRemote(remoteParent(strings.TrimSpace(getText(w.ctlRemotePath))))
					} else if sel.IsDir {
						w.refreshRemote(remoteJoin(w.remotePath, sel.Name))
					} else if isKnownBinaryExt(filepath.Ext(sel.Name)) {
						// File biner yang sudah dikenal: Enter /
						// double-click tetap download (perilaku lama).
						w.startDownload()
					} else {
						// File teks (json, conf, txt, dll, plus
						// ekstensi tak dikenal yang ternyata teks
						// setelah dicek isinya): Enter / double-click
						// membuka editor, Save meng-upload balik.
						// Tombol Download di tengah tetap untuk
						// mengunduh file teks tanpa mengeditnya.
						w.startEdit(remoteJoin(w.remotePath, sel.Name), sel.Name)
					}
				}
			}
		}
		return 0
	case win.WM_CONTEXTMENU:
		// Klik kanan: hanya panel remote (server) yang punya menu.
		// wParam = HWND kontrol yang diklik, lParam = koordinat layar.
		// Kontrol lain (mis. Edit path) dibiarkan ke DefWindowProc
		// supaya menu bawaan Edit (Cut/Copy/Paste) tetap jalan.
		if w != nil && win.HWND(wParam) == w.ctlRemoteList {
			sx := int32(int16(lParam & 0xFFFF))
			sy := int32(int16((lParam >> 16) & 0xFFFF))
			w.sftpShowRemoteMenu(sx, sy)
			return 0
		}
		return win.DefWindowProc(hwnd, msg, wParam, lParam)
	case wmAppSFTPInputDone:
		if w == nil {
			return 0
		}
		res := takeInputResult(wParam)
		if res == nil || res.cancelled {
			return 0
		}
		if res.mode == "rename" {
			oldName := remoteBase(res.oldPath)
			if res.newName == oldName {
				w.appendLog("Rename cancelled: name unchanged.")
				return 0
			}
			newPath := remoteJoin(remoteParent(res.oldPath), res.newName)
			w.appendLog("Renaming: " + oldName + " -> " + res.newName)
			w.sftpRunOp("rename", res.newName, func(client *sftp.Client) error {
				return client.Rename(res.oldPath, newPath)
			})
		} else if res.mode == "newfolder" {
			newPath := remoteJoin(res.oldPath, res.newName)
			w.appendLog("Creating folder: " + res.newName)
			w.sftpRunOp("newfolder", res.newName, func(client *sftp.Client) error {
				return client.Mkdir(newPath)
			})
		}
		return 0
	case wmAppSFTPOpDone:
		if w == nil {
			return 0
		}
		op := takeSFTPOp(wParam)
		if op == nil {
			return 0
		}
		// Hasil Edit: "edit-binary" bukan error — file ternyata
		// biner setelah dicek isinya, jadi tidak dibuka di editor.
		// Beri tahu user pakai tombol Download, tanpa MessageBox
		// error (file tidak rusak, tidak ada yang gagal).
		if op.op == "edit-binary" {
			w.appendLog("Binary file detected, not opening in editor: " + op.name + ". Use the Download button to download it.")
			msgBox(hwnd, "This file looks like a binary file, not text, so it was not opened in the editor.\n\nUse the Download button if you want to download it.", "SFTP", win.MB_ICONWARNING|win.MB_OK)
			return 0
		}
		// "edit-open" sudah dilog saat startEdit ("Editing: ..."),
		// jadi di sini cukup log editornya tanpa refresh list yang
		// tidak perlu (file belum berubah).
		if op.op == "edit-open" {
			w.appendLog("Opened in editor: " + op.name + ". Save in the editor to upload it back to the server.")
			return 0
		}
		// Upload balik Edit (watcher, op "upload") BUKAN transfer UI
		// biasa — transfer biasa lewat wmAppSFTPDone yang mengisi
		// bar sampai 100, sedangkan op ini lewat sini dan tadinya
		// tidak menyentuh bar sama sekali, jadi user hanya melihat
		// log "Upload finished" tanpa bar finish. Perbaiki: isi bar
		// sampai 100 + label finish, sama seperti transfer biasa.
		// Kalau ada transfer UI yang sedang jalan, JANGAN sentuh
		// bar/label (punya transfer itu, jangan ditimpa).
		if op.op == "upload" && !w.transferInProg {
			if op.err == nil {
				if w.ctlProgress != 0 {
					win.SendMessage(w.ctlProgress, win.PBM_SETPOS, 100, 0)
				}
				setText(w.ctlProgressLabel, "Upload finished: "+op.name)
			} else {
				if w.ctlProgress != 0 {
					win.SendMessage(w.ctlProgress, win.PBM_SETPOS, 0, 0)
				}
				setText(w.ctlProgressLabel, "Failed: "+op.name)
			}
		}
		if op.err == nil {
			w.appendLog(capFirst(op.op) + " finished: " + op.name)
			if w.client != nil {
				w.refreshRemote(w.remotePath)
			}
		} else {
			w.appendLog(capFirst(op.op) + " failed: " + op.name + ": " + op.err.Error())
			msgBox(hwnd, capFirst(op.op)+" failed: "+op.err.Error(), "SFTP", win.MB_ICONWARNING|win.MB_OK)
		}
		return 0
	case win.WM_CLOSE:
		if w != nil {
			sftpClose(w)
		}
		win.DestroyWindow(hwnd)
		return 0
	case win.WM_DESTROY:
		if w != nil {
			sftpClose(w)
			delete(sftpWins, hwnd)
		}
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}
