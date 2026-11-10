//go:build windows

// Dialog pemilih terminal untuk tombol SFTP di panel sesi.
// Dipakai HANYA kalau terminal SSH yang terhubung lebih dari satu:
// kalau cuma satu, SFTP langsung dibuka tanpa pemilih (tidak nambah
// klik); kalau banyak, user memilih dari daftar ini, bukan ditebak
// dari form sesi (tebakan form gagal untuk duplikat user@host yang
// sama dan untuk form yang tidak sedang terhubung).
// Pola jendela mengikuti dialog input SFTP: bukan dialog Win32
// beneran, parent di-disable selama terbuka, hasil dikirim async
// lewat WM_APP ke panel sesi.
package main

import (
	"syscall"
	"unsafe"

	"github.com/lxn/win"
)

const (
	pickIdLabel = 6000 + iota
	pickIdList
	pickIdOK
	pickIdCancel
)

const wmAppSFTPPickDone = win.WM_APP + 18

type pickWin struct {
	hwnd        win.HWND
	parent      win.HWND
	list        win.HWND
	terms       []*termWin
	tabControls []win.HWND
}

// pickWindowForChild: dialog pemilih pemilik sebuah HWND.
func pickWindowForChild(hwnd win.HWND) *pickWin {
	if p := pickWins[hwnd]; p != nil {
		return p
	}
	parent := win.GetParent(hwnd)
	if parent != 0 {
		return pickWins[parent]
	}
	return nil
}

// pickHandleKey: Tab / Enter / Escape di dialog pemilih. True kalau
// ditangani (message loop harus skip Dispatch). Up/Down di ListBox
// TIDAK dicegat di sini — ListBox menanganinya sendiri untuk pindah
// seleksi, persis seperti yang diminta user untuk Enter folder.
func pickHandleKey(msg *win.MSG) bool {
	if msg.Message != win.WM_KEYDOWN {
		return false
	}
	p := pickWindowForChild(msg.HWnd)
	if p == nil || len(p.tabControls) == 0 {
		return false
	}
	switch msg.WParam {
	case win.VK_TAB:
		focus := win.GetFocus()
		idx := -1
		for i, h := range p.tabControls {
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
			next = (idx - 1 + len(p.tabControls)) % len(p.tabControls)
		} else {
			next = (idx + 1) % len(p.tabControls)
		}
		win.SetFocus(p.tabControls[next])
		return true
	case win.VK_RETURN:
		win.SendMessage(p.hwnd, win.WM_COMMAND, uintptr((win.BN_CLICKED<<16)|pickIdOK), 0)
		return true
	case win.VK_ESCAPE:
		win.SendMessage(p.hwnd, win.WM_COMMAND, uintptr((win.BN_CLICKED<<16)|pickIdCancel), 0)
		return true
	}
	return false
}

var (
	pickWins     = map[win.HWND]*pickWin{}
	pickProc     = syscall.NewCallback(pickWndProc)
	pendingPick  *pickWin
	pickLabels   []string
)

// openPickDialog: buka dialog pemilih untuk terms (sudah terfilter
// SSH terhubung, urut dibuka). Dialog di tengah panel sesi.
func openPickDialog(parent win.HWND, terms []*termWin) {
	labels := make([]string, 0, len(terms))
	for _, t := range terms {
		promptDir := ""
		if t.term != nil {
			promptDir = t.term.PromptDir()
		}
		labels = append(labels, formatPickLabel(t.cfg.Name, termWho(t), promptDir))
	}
	p := &pickWin{parent: parent, terms: terms}
	pickLabels = labels
	pendingPick = p
	dlgX, dlgY := int32(win.CW_USEDEFAULT), int32(win.CW_USEDEFAULT)
	var prc win.RECT
	if win.GetWindowRect(parent, &prc) {
		pw := prc.Right - prc.Left
		ph := prc.Bottom - prc.Top
		dlgX = prc.Left + (pw-480)/2
		dlgY = prc.Top + (ph-320)/2
		if dlgX < 0 {
			dlgX = 0
		}
		if dlgY < 0 {
			dlgY = 0
		}
	}
	hwnd := win.CreateWindowEx(0, utf16("SSHTermPick"), utf16("Select terminal for SFTP"),
		win.WS_OVERLAPPEDWINDOW, dlgX, dlgY, 480, 320,
		parent, 0, hInst, nil)
	pendingPick = nil
	if hwnd == 0 {
		return
	}
	p.hwnd = hwnd
	pickWins[hwnd] = p
	setWindowIcon(hwnd)
	win.EnableWindow(parent, false)
	win.ShowWindow(hwnd, win.SW_SHOW)
	win.UpdateWindow(hwnd)
}

func pickWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	p := pickWins[hwnd]
	switch msg {
	case win.WM_CREATE:
		if p == nil {
			p = pendingPick
			if p != nil {
				p.hwnd = hwnd
				pickWins[hwnd] = p
			}
		}
		if p == nil {
			return 0
		}
		createControl("STATIC", "Select a terminal for SFTP:", 0, 16, 16, 432, 22, hwnd, pickIdLabel)
		p.list = createControl("LISTBOX", "", win.WS_VSCROLL|win.LBS_NOTIFY, 16, 44, 432, 180, hwnd, pickIdList)
		for _, label := range pickLabels {
			win.SendMessage(p.list, win.LB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16(label))))
		}
		// Seleksi awal = terminal pertama (urut dibuka), supaya
		// Enter langsung bekerja tanpa klik dulu.
		if len(pickLabels) > 0 {
			win.SendMessage(p.list, win.LB_SETCURSEL, 0, 0)
		}
		okBtn := createControl("BUTTON", "OK", win.BS_DEFPUSHBUTTON, 272, 240, 80, 28, hwnd, pickIdOK)
		cancelBtn := createControl("BUTTON", "Cancel", 0, 368, 240, 80, 28, hwnd, pickIdCancel)
		p.tabControls = []win.HWND{p.list, okBtn, cancelBtn}
		cb := syscall.NewCallback(func(child win.HWND, _ uintptr) uintptr {
			win.SendMessage(child, win.WM_SETFONT, uintptr(uiFont), 1)
			return 1
		})
		fontEnumCallback = cb
		win.EnumChildWindows(hwnd, cb, 0)
		win.SetFocus(p.list)
		return 0
	case win.WM_COMMAND:
		if p == nil {
			return 0
		}
		id := int32(wParam & 0xFFFF)
		code := int32(wParam >> 16)
		// Double-click di list = pilih + OK.
		if id == pickIdList && code == win.LBN_DBLCLK {
			id = pickIdOK
			code = win.BN_CLICKED
		}
		if code == win.BN_CLICKED && (id == pickIdOK || id == pickIdCancel) {
			var chosen win.HWND
			if id == pickIdOK {
				sel := int(win.SendMessage(p.list, win.LB_GETCURSEL, 0, 0))
				if sel < 0 || sel >= len(p.terms) {
					return 0
				}
				chosen = p.terms[sel].hwnd
			}
			parent := p.parent
			win.EnableWindow(parent, true)
			win.DestroyWindow(hwnd)
			postMessage(parent, wmAppSFTPPickDone, uintptr(chosen), 0)
			return 0
		}
		return 0
	case win.WM_CLOSE:
		if p != nil {
			parent := p.parent
			win.EnableWindow(parent, true)
			win.DestroyWindow(hwnd)
			postMessage(parent, wmAppSFTPPickDone, 0, 0)
		}
		return 0
	case win.WM_DESTROY:
		if p != nil {
			delete(pickWins, hwnd)
		}
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}
