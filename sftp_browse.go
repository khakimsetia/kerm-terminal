//go:build windows

// Dialog pilih folder untuk panel lokal SFTP (2026-11-16, atas
// permintaan user): tombol "📁 Browse" di sebelah Up di panel kiri,
// membuka dialog pilih folder Windows (SHBrowseForFolder) supaya
// user bisa explore ke folder mana pun tanpa mengetik path.
//
// Dialog dibuka di folder lokal saat ini (startDir) lewat callback
// BFFM_INITIALIZED -> BFFM_SETSELECTION. Kalau startDir kosong /
// tidak ada, dialog dibuka di posisi default Windows (tanpa error).
// Hanya folder filesystem yang bisa dipilih (BIF_RETURNONLYFSDIRS),
// gaya dialog baru (BIF_NEWDIALOGSTYLE), tanpa tombol "Make New
// Folder" (ini untuk navigasi, bukan membuat folder).
package main

import (
	"runtime"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

const (
	bifReturnOnlyFSDirs  = 0x00000001
	bifNewDialogStyle    = 0x00000040
	bifNoNewFolderButton = 0x00000200
	bffmInitialized      = 1
	bffmSetSelection     = 0x400 + 103 // BFFM_SETSELECTIONW (WM_USER + 103)
)

// browseCallbackProc: callback SHBrowseForFolder (hwnd, uMsg, lParam,
// lpData). Saat dialog siap (BFFM_INITIALIZED), pilih folder awal
// dari lpData (= pointer path UTF16 yang dikirim lewat BROWSEINFO.LParam).
var browseCallbackProc = syscall.NewCallback(func(hwnd, uMsg, lParam, lpData uintptr) uintptr {
	if uMsg == bffmInitialized {
		win.SendMessage(win.HWND(hwnd), bffmSetSelection, 1, lpData)
	}
	return 0
})

// browseLocalFolder: dialog pilih folder Windows. Dibuka di startDir.
// Kembalikan path yang dipilih, atau "" kalau dibatalkan.
func browseLocalFolder(owner win.HWND, startDir string) string {
	title, _ := windows.UTF16FromString("Select folder")
	displayName := make([]uint16, 260)
	startUTF16, _ := windows.UTF16FromString(startDir)
	bi := win.BROWSEINFO{
		HwndOwner:      owner,
		PszDisplayName: &displayName[0],
		LpszTitle:      &title[0],
		UlFlags:        bifReturnOnlyFSDirs | bifNewDialogStyle | bifNoNewFolderButton,
		Lpfn:           browseCallbackProc,
		LParam:         uintptr(unsafe.Pointer(&startUTF16[0])),
	}
	pidl := win.SHBrowseForFolder(&bi)
	// startUTF16 hanya terlihat lewat LParam (uintptr, tak terlihat
	// GC): tahan sampai dialog selesai.
	runtime.KeepAlive(startUTF16)
	if pidl == 0 {
		return ""
	}
	defer win.CoTaskMemFree(pidl)
	pathBuf := make([]uint16, 260)
	if !win.SHGetPathFromIDList(pidl, &pathBuf[0]) {
		return ""
	}
	return windows.UTF16ToString(pathBuf)
}
