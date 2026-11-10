//go:build windows

package main

import (
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

var (
	procSetWindowTextW      = windows.NewLazySystemDLL("user32.dll").NewProc("SetWindowTextW")
	procGetWindowTextW      = windows.NewLazySystemDLL("user32.dll").NewProc("GetWindowTextW")
	procGetWindowTextLength = windows.NewLazySystemDLL("user32.dll").NewProc("GetWindowTextLengthW")
	procPostMessageW        = windows.NewLazySystemDLL("user32.dll").NewProc("PostMessageW")
	procCreateSolidBrush    = windows.NewLazySystemDLL("gdi32.dll").NewProc("CreateSolidBrush")
	procGetTextFaceW        = windows.NewLazySystemDLL("gdi32.dll").NewProc("GetTextFaceW")
	procFillRect            = windows.NewLazySystemDLL("user32.dll").NewProc("FillRect")
)

func setWindowText(hwnd win.HWND, s string) {
	p, _ := windows.UTF16PtrFromString(s)
	procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(p)))
}

func getWindowText(hwnd win.HWND) string {
	n, _, _ := procGetWindowTextLength.Call(uintptr(hwnd))
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(n+1))
	return windows.UTF16ToString(buf)
}

func postMessage(hwnd win.HWND, msg uint32, wParam, lParam uintptr) {
	procPostMessageW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
}

// getTextFace returns the actual font face selected into hdc, used to
// verify that a requested font really exists (Windows font mapper
// silently substitutes a different face otherwise).
func getTextFace(hdc win.HDC) string {
	buf := make([]uint16, 64)
	n, _, _ := procGetTextFaceW.Call(uintptr(hdc), uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if n <= 0 || int(n) > len(buf) {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

func createSolidBrush(color win.COLORREF) win.HBRUSH {
	r, _, _ := procCreateSolidBrush.Call(uintptr(color))
	return win.HBRUSH(r)
}

func fillRect(hdc win.HDC, rc *win.RECT, brush win.HBRUSH) {
	procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(rc)), uintptr(brush))
}

func rgb(r, g, b uint32) win.COLORREF {
	return win.COLORREF(r | g<<8 | b<<16)
}

func colorFromUint32(c uint32) win.COLORREF {
	return rgb(c>>16&0xFF, c>>8&0xFF, c&0xFF)
}
