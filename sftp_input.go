//go:build windows

// Dialog input kecil untuk SFTP: Rename dan New folder.
// Satu jendela input, bukan MessageBox (MessageBox tidak bisa input
// teks). Hasil dikirim ke jendela SFTP lewat WM_APP + store map,
// mengikuti pola async SFTP lainnya — dialog tidak memblokir message
// loop utama, dan parent SFTP di-disable selama dialog terbuka
// (perilaku modal, tanpa local message loop).
package main

import (
	"strings"
	"syscall"

	"github.com/lxn/win"
)

const (
	inputIdLabel  = 5000 + iota
	inputIdEdit
	inputIdOK
	inputIdCancel
)

const wmAppSFTPInputDone = win.WM_APP + 15

type inputDlg struct {
	hwnd   win.HWND
	parent win.HWND
	edit   win.HWND
	mode   string // "rename" atau "newfolder"
	oldPath string
	tabControls []win.HWND
}

// inputWindowForChild: dialog input pemilik sebuah HWND.
func inputWindowForChild(hwnd win.HWND) *inputDlg {
	if d := inputWins[hwnd]; d != nil {
		return d
	}
	parent := win.GetParent(hwnd)
	if parent != 0 {
		return inputWins[parent]
	}
	return nil
}

// inputHandleKey: Tab / Enter / Escape di dialog input. True kalau
// ditangani (message loop harus skip Dispatch). Tanpa ini, Enter di
// Edit tidak mengklik OK dan Tab tidak pindah fokus, karena jendela
// ini bukan dialog Win32 beneran (tanpa IsDialogMessage).
func inputHandleKey(msg *win.MSG) bool {
	if msg.Message != win.WM_KEYDOWN {
		return false
	}
	d := inputWindowForChild(msg.HWnd)
	if d == nil || len(d.tabControls) == 0 {
		return false
	}
	switch msg.WParam {
	case win.VK_TAB:
		focus := win.GetFocus()
		idx := -1
		for i, h := range d.tabControls {
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
			next = (idx - 1 + len(d.tabControls)) % len(d.tabControls)
		} else {
			next = (idx + 1) % len(d.tabControls)
		}
		win.SetFocus(d.tabControls[next])
		return true
	case win.VK_RETURN:
		// Enter di Edit (atau di tombol) = OK.
		win.SendMessage(d.hwnd, win.WM_COMMAND, uintptr((win.BN_CLICKED<<16)|inputIdOK), 0)
		return true
	case win.VK_ESCAPE:
		win.SendMessage(d.hwnd, win.WM_COMMAND, uintptr((win.BN_CLICKED<<16)|inputIdCancel), 0)
		return true
	}
	return false
}

type inputResult struct {
	mode     string
	oldPath  string
	newName  string
	cancelled bool
}

var (
	inputWins      = map[win.HWND]*inputDlg{}
	inputProc      = syscall.NewCallback(inputWndProc)
	pendingInput   *inputDlg
	inputResultByID = map[uintptr]*inputResult{}
)

func storeInputResult(r *inputResult) uintptr {
	msgMu.Lock()
	defer msgMu.Unlock()
	id := msgNextID
	msgNextID++
	inputResultByID[id] = r
	return id
}

func takeInputResult(id uintptr) *inputResult {
	msgMu.Lock()
	defer msgMu.Unlock()
	r := inputResultByID[id]
	delete(inputResultByID, id)
	return r
}

// openInputDialog: buka dialog input untuk SFTP. title = judul jendela,
// label = teks di atas Edit, initial = isi awal Edit (nama lama untuk
// Rename, kosong untuk New folder).
func openInputDialog(parent win.HWND, title, label, initial, mode, oldPath string) {
	d := &inputDlg{parent: parent, mode: mode, oldPath: oldPath}
	// Label + initial HARUS di-set SEBELUM CreateWindowEx, karena
	// WM_CREATE (di dalam CreateWindowEx) langsung membuat Edit
	// dengan teks itu.
	inputLabel = label
	inputInitial = initial
	pendingInput = d
	// Popup di TENGAH jendela SFTP (kalau SFTP di-maximize, itu
	// berarti tengah layar). Dulu pakai CW_USEDEFAULT, jadi muncul
	// di pojok / posisi default Windows yang tidak tentu.
	dlgX, dlgY := int32(win.CW_USEDEFAULT), int32(win.CW_USEDEFAULT)
	var prc win.RECT
	if win.GetWindowRect(parent, &prc) {
		pw := prc.Right - prc.Left
		ph := prc.Bottom - prc.Top
		dlgX = prc.Left + (pw-420)/2
		dlgY = prc.Top + (ph-170)/2
		if dlgX < 0 {
			dlgX = 0
		}
		if dlgY < 0 {
			dlgY = 0
		}
	}
	hwnd := win.CreateWindowEx(0, utf16("SSHTermInput"), utf16(title),
		win.WS_OVERLAPPEDWINDOW, dlgX, dlgY, 420, 170,
		parent, 0, hInst, nil)
	pendingInput = nil
	if hwnd == 0 {
		return
	}
	d.hwnd = hwnd
	inputWins[hwnd] = d
	setWindowIcon(hwnd)
	win.EnableWindow(parent, false)
	win.ShowWindow(hwnd, win.SW_SHOW)
	win.UpdateWindow(hwnd)
}

// Variabel sementara untuk initial/label dialog input. Aman karena
// WM_CREATE jalan sinkron di dalam CreateWindowEx (pola yang sama
// dengan pendingSFTPWin).
var inputInitial, inputLabel string

func inputWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	d := inputWins[hwnd]
	switch msg {
	case win.WM_CREATE:
		if d == nil {
			d = pendingInput
			if d != nil {
				d.hwnd = hwnd
				inputWins[hwnd] = d
			}
		}
		if d == nil {
			return 0
		}
		createControl("STATIC", inputLabel, 0, 16, 16, 368, 22, hwnd, inputIdLabel)
		d.edit = createControl("EDIT", inputInitial, win.WS_BORDER|win.ES_AUTOHSCROLL, 16, 44, 368, 24, hwnd, inputIdEdit)
		okBtn := createControl("BUTTON", "OK", win.BS_DEFPUSHBUTTON, 200, 84, 80, 28, hwnd, inputIdOK)
		cancelBtn := createControl("BUTTON", "Cancel", 0, 292, 84, 80, 28, hwnd, inputIdCancel)
		d.tabControls = []win.HWND{d.edit, okBtn, cancelBtn}
		cb := syscall.NewCallback(func(child win.HWND, _ uintptr) uintptr {
			win.SendMessage(child, win.WM_SETFONT, uintptr(uiFont), 1)
			return 1
		})
		fontEnumCallback = cb
		win.EnumChildWindows(hwnd, cb, 0)
		win.SetFocus(d.edit)
		// Pilih semua teks (untuk Rename, user tinggal ketik nama baru).
		win.SendMessage(d.edit, win.EM_SETSEL, 0, ^uintptr(0))
		return 0
	case win.WM_COMMAND:
		if d == nil {
			return 0
		}
		id := int32(wParam & 0xFFFF)
		code := int32(wParam >> 16)
		if code == win.BN_CLICKED && (id == inputIdOK || id == inputIdCancel) {
			cancelled := id == inputIdCancel
			newName := ""
			if !cancelled {
				newName = strings.TrimSpace(getText(d.edit))
				if err := validateNewName(newName); err != nil {
					msgBox(hwnd, "Invalid name: "+err.Error(), "SFTP", win.MB_ICONWARNING|win.MB_OK)
					win.SetFocus(d.edit)
					return 0
				}
			}
			parent := d.parent
			mode := d.mode
			oldPath := d.oldPath
			win.EnableWindow(parent, true)
			win.DestroyWindow(hwnd)
			postMessage(parent, wmAppSFTPInputDone, storeInputResult(&inputResult{mode: mode, oldPath: oldPath, newName: newName, cancelled: cancelled}), 0)
			return 0
		}
		return 0
	case win.WM_CLOSE:
		if d != nil {
			parent := d.parent
			mode := d.mode
			oldPath := d.oldPath
			win.EnableWindow(parent, true)
			win.DestroyWindow(hwnd)
			postMessage(parent, wmAppSFTPInputDone, storeInputResult(&inputResult{mode: mode, oldPath: oldPath, cancelled: true}), 0)
		}
		return 0
	case win.WM_DESTROY:
		if d != nil {
			delete(inputWins, hwnd)
		}
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}
