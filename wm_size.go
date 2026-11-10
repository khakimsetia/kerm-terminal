// Helper WM_SIZE murni Go (tanpa dependensi Win32) supaya bisa
// di-unit-test di Linux, dipakai oleh termWndProc di main_windows.go
// (yang hanya di-build untuk Windows).
package main

// wmSizeMinimized adalah nilai wParam WM_SIZE untuk SIZE_MINIMIZED.
// lxn/win tidak mendefinisikan konstanta SIZE_*, jadi ditulis literal
// dengan komentar ini. Nilai Win32: 0=RESTORED, 1=MINIMIZED,
// 2=MAXIMIZED, 3=MAXSHOW, 4=MAXHIDE.
const wmSizeMinimized = 1

// shouldSkipTermResize mengembalikan true kalau WM_SIZE ini adalah
// minimize. Bug 2026-10-29: saat jendela di-minimize, Windows mengirim
// WM_SIZE dengan ukuran klien 0x0. Handler lama selalu memanggil
// applyTermSize, sehingga termColsRows mengembalikan minimum
// 20x5 dan Term.Resize(20, 5) memotong buffer dari ukuran semula
// (mis. 97x30) menjadi 20 kolom x 5 baris — sebagian besar isi
// terminal hilang permanen, dan WindowChange 20x5 juga dikirim ke
// server. Saat restore ke ukuran semula, isi yang sudah terpotong
// itu tidak bisa kembali. Minimize bukan perubahan ukuran terminal
// sama sekali (hanya visibilitas), jadi harus dilewati total: buffer,
// scrollback, dan ukuran PTY server tidak boleh berubah.
func shouldSkipTermResize(wParam uintptr) bool {
	return wParam == wmSizeMinimized
}
