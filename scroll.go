// Helper scroll murni Go (tanpa dependensi Win32) supaya bisa
// di-unit-test di Linux. Dipakai handler WM_VSCROLL dan
// updateTermScrollbar di main_windows.go.
package main

// scrollOffsetFromThumbPos: offset scroll dari posisi scroll pos.
// pos 0 = paling atas (offset maksimum = seluruh scrollback),
// pos == sb = live (offset 0, paling bawah). Hasil di-clamp
// ke 0..sb. Argumen keduanya adalah POSISI SCROLL (bukan posisi
// mentah thumb); konversi thumb mentah -> pos ada di
// scrollThumbPosToPos di bawah.
func scrollOffsetFromThumbPos(sb, pos int) int {
	off := sb - pos
	if off < 0 {
		return 0
	}
	if off > sb {
		return sb
	}
	return off
}

// scrollbarRange: jarak gerak thumb (travelMax) dan ukuran page
// untuk scrollbar, dari panjang scrollback sb dan baris terlihat
// rows. Posisi thumb berkisar 0..travelMax, 0 = paling atas,
// travelMax = live (paling bawah).
//
// Untuk sb <= 65535 pemetaannya persis (travelMax = sb,
// page = rows), sama seperti sebelum perubahan ini.
// Untuk sb yang lebih besar, jarak gerak diskalakan ke 65535
// supaya posisi thumb di wParam WM_VSCROLL (16 bit) tetap
// bermakna: tanpa penskalaan, posisi >65535 terpotong dan
// thumb tidak pernah sampai ke bawah dengan benar.
//
// PENSKALAAN INI ADA KARENA BUG 2026-11-13: scrollbar selalu
// kembali ke atas. Dua percobaan sebelumnya mengandalkan
// GetScrollInfo (NTrackPos saat drag, SIF_POS saat lepas) dan
// ternyata tidak bisa diandalkan di sini — thumb tetap kembali
// ke atas. Mulai sekarang posisi thumb HANYA dibaca dari wParam
// pesan WM_VSCROLL itu sendiri (sumber yang dijamin Windows),
// dan jarak geraknya dijaga <= 65535 dengan penskalaan di sini,
// jadi tidak ada lagi ketergantungan pada GetScrollInfo.
func scrollbarRange(sb, rows int) (travelMax, page int) {
	if rows < 1 {
		rows = 1
	}
	if sb <= 65535 {
		return sb, rows
	}
	page = rows * 65535 / sb
	if page < 1 {
		page = 1
	}
	return 65535, page
}

// scrollPosToThumbPos: posisi thumb (NPos) untuk posisi scroll pos
// (0 = atas, sb = live). Kebalikan dari scrollThumbPosToPos.
func scrollPosToThumbPos(sb, rows, pos int) int {
	travelMax, _ := scrollbarRange(sb, rows)
	if sb <= 0 || travelMax <= 0 {
		return 0
	}
	if pos < 0 {
		pos = 0
	}
	if pos > sb {
		pos = sb
	}
	return pos * travelMax / sb
}

// scrollThumbPosToPos: posisi scroll pos dari posisi thumb mentah
// (0..travelMax). Kebalikan dari scrollPosToThumbPos.
func scrollThumbPosToPos(sb, rows, thumbPos int) int {
	travelMax, _ := scrollbarRange(sb, rows)
	if travelMax <= 0 {
		return 0
	}
	if thumbPos < 0 {
		thumbPos = 0
	}
	if thumbPos > travelMax {
		thumbPos = travelMax
	}
	return thumbPos * sb / travelMax
}
