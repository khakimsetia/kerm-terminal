//go:build windows

// Dialog timpa gaya WinSCP untuk SFTP (2026-11-16, atas permintaan user).
//
// Per file yang sudah ada di tujuan, dialog menampilkan ukuran +
// tanggal file baru vs yang sudah ada, dengan tombol:
//   Yes        = timpa file ini saja
//   Yes to All = timpa file ini DAN semua file berikut yang sudah ada
//   No         = lewati file ini saja
//   No to All  = lewati file ini DAN semua file berikut yang sudah ada
//   Cancel     = batalkan seluruh transfer
// Sengaja TANPA Newer only / New name (permintaan user).
//
// Berbeda dengan dialog input (Rename / New folder) yang async,
// dialog ini MODAL dan sinkron: askOverwriteWinSCP menjalankan
// message loop sendiri sampai user menjawab, lalu mengembalikan
// jawabannya — karena keputusan per file harus diketahui SEBELUM
// antrean transfer jalan (logika "to All" ada di
// applyOverwriteDecisions, sftp.go). Loop modal memompa SEMUA pesan
// thread (terminal tetap jalan, output tetap tampil); hanya jendela
// SFTP pemilik yang di-disable selama dialog terbuka. Tab / Enter
// (tombol default Yes) / Escape (Cancel) ditangani IsDialogMessage,
// seperti jendela sesi.
package main

import (
	"syscall"
	"time"

	"github.com/lxn/win"
)

const (
	overwriteIdLabel = 6000 + iota
	overwriteIdText
	overwriteIdYes
	overwriteIdYesAll
	overwriteIdNo
	overwriteIdNoAll
	overwriteIdCancel
)

type overwriteDlg struct {
	hwnd   win.HWND
	parent win.HWND
	result overwriteDecision // default Cancel (aman: jangan timpa)
	done   bool
	yesBtn win.HWND
}

var (
	overwriteWins      = map[win.HWND]*overwriteDlg{}
	overwriteProc      = syscall.NewCallback(overwriteWndProc)
	pendingOverwrite   *overwriteDlg
	overwriteText      string // teks dialog; aman, WM_CREATE jalan sinkron
)

// askOverwriteWinSCP: tampilkan dialog timpa WinSCP untuk satu file,
// tunggu jawaban user (modal), kembalikan jawabannya. Kalau dialog
// gagal dibuat, jawabannya No (jangan timpa) — aman.
func askOverwriteWinSCP(owner win.HWND, name, target string, existingSize int64, existingTime time.Time, newSize int64, newTime time.Time) overwriteDecision {
	d := &overwriteDlg{parent: owner, result: overwriteCancel}
	overwriteText = overwriteDialogText(name, target, existingSize, existingTime, newSize, newTime)
	pendingOverwrite = d
	// Popup di TENGAH jendela SFTP (pola dialog input).
	dlgX, dlgY := int32(win.CW_USEDEFAULT), int32(win.CW_USEDEFAULT)
	var prc win.RECT
	if win.GetWindowRect(owner, &prc) {
		pw := prc.Right - prc.Left
		ph := prc.Bottom - prc.Top
		dlgX = prc.Left + (pw-580)/2
		dlgY = prc.Top + (ph-220)/2
		if dlgX < 0 {
			dlgX = 0
		}
		if dlgY < 0 {
			dlgY = 0
		}
	}
	hwnd := win.CreateWindowEx(0, utf16("SSHTermOverwrite"), utf16("Overwrite?"),
		win.WS_OVERLAPPEDWINDOW, dlgX, dlgY, 580, 220,
		owner, 0, hInst, nil)
	pendingOverwrite = nil
	if hwnd == 0 {
		return overwriteNo
	}
	d.hwnd = hwnd
	overwriteWins[hwnd] = d
	setWindowIcon(hwnd)
	win.EnableWindow(owner, false)
	win.ShowWindow(hwnd, win.SW_SHOW)
	win.UpdateWindow(hwnd)
	var msg win.MSG
	for !d.done {
		ret := win.GetMessage(&msg, 0, 0, 0)
		if ret == 0 || ret == -1 {
			// WM_QUIT / error: batalkan.
			d.result = overwriteCancel
			break
		}
		if msg.HWnd == hwnd || win.IsChild(hwnd, msg.HWnd) {
			if win.IsDialogMessage(hwnd, &msg) {
				continue
			}
		}
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
		// Dicek di sini (bukan hanya di atas loop): WM_DESTROY
		// dialog jalan sinkron di dalam DestroyWindow, jadi d.done
		// sudah true begitu dispatch kembali — jangan menunggu pesan
		// berikutnya. Penghancuran dari luar (parent ditutup) juga
		// lewat WM_DESTROY ini, jadi tidak ada jalan gantung.
		if d.done {
			break
		}
	}
	win.EnableWindow(owner, true)
	win.SetFocus(owner)
	return d.result
}

func overwriteWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	d := overwriteWins[hwnd]
	switch msg {
	case win.WM_CREATE:
		if d == nil {
			d = pendingOverwrite
			if d != nil {
				d.hwnd = hwnd
				overwriteWins[hwnd] = d
			}
		}
		if d == nil {
			return 0
		}
		createControl("STATIC", overwriteText, 0, 16, 16, 548, 104, hwnd, overwriteIdText)
		// Lima tombol: Yes (default, Enter) + Yes to All + No +
		// No to All + Cancel (Escape via IsDialogMessage -> IDCANCEL).
		d.yesBtn = createControl("BUTTON", "Yes", win.BS_DEFPUSHBUTTON, 16, 138, 98, 28, hwnd, overwriteIdYes)
		createControl("BUTTON", "Yes to All", 0, 122, 138, 110, 28, hwnd, overwriteIdYesAll)
		createControl("BUTTON", "No", 0, 240, 138, 96, 28, hwnd, overwriteIdNo)
		createControl("BUTTON", "No to All", 0, 344, 138, 110, 28, hwnd, overwriteIdNoAll)
		createControl("BUTTON", "Cancel", 0, 462, 138, 98, 28, hwnd, overwriteIdCancel)
		cb := syscall.NewCallback(func(child win.HWND, _ uintptr) uintptr {
			win.SendMessage(child, win.WM_SETFONT, uintptr(uiFont), 1)
			return 1
		})
		fontEnumCallback = cb
		win.EnumChildWindows(hwnd, cb, 0)
		win.SetFocus(d.yesBtn)
		return 0
	case win.WM_COMMAND:
		if d == nil {
			return 0
		}
		id := int32(wParam & 0xFFFF)
		code := int32(wParam >> 16)
		// Escape (IsDialogMessage) = IDCANCEL, tanpa peduli code.
		if id == win.IDCANCEL {
			d.result = overwriteCancel
			win.DestroyWindow(hwnd)
			return 0
		}
		if code == win.BN_CLICKED {
			switch id {
			case overwriteIdYes:
				d.result = overwriteYes
			case overwriteIdYesAll:
				d.result = overwriteYesAll
			case overwriteIdNo:
				d.result = overwriteNo
			case overwriteIdNoAll:
				d.result = overwriteNoAll
			case overwriteIdCancel:
				d.result = overwriteCancel
			default:
				return 0
			}
			win.DestroyWindow(hwnd)
			return 0
		}
		return 0
	case win.WM_CLOSE:
		if d != nil {
			d.result = overwriteCancel
			win.DestroyWindow(hwnd)
		}
		return 0
	case win.WM_DESTROY:
		if d != nil {
			// result sudah terisi (default Cancel), tinggal tandai selesai.
			d.done = true
			delete(overwriteWins, hwnd)
		}
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}
