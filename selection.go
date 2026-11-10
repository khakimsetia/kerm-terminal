// Logika seleksi murni Go (tanpa Win32) supaya bisa di-unit-test di
// Linux. Dipakai oleh termWndProc di main_windows.go.
//
// Bug 2026-10-29: blok teks yang panjangnya melebihi satu layar
// (sampai scroll) tidak bisa dipilih sampai semuanya. Penyebabnya ada
// tiga, semuanya diperbaiki bersama:
//  1. Tanpa mouse capture, drag yang keluar dari jendela berhenti
//     menerima WM_MOUSEMOVE — seleksi mentok di baris terakhir yang
//     terlihat.
//  2. Tanpa auto-scroll, drag melewati tepi atas/bawah tidak pernah
//     menggeser tampilan — seleksi tidak bisa masuk ke scrollback
//     (ke atas) atau kembali ke live (ke bawah).
//  3. Koordinat seleksi lama bersifat view-relative (indeks baris di
//     LinesForView). Begitu scrollOffset berubah — karena auto-scroll,
//     wheel, atau output baru — indeks yang sama menunjuk ke baris
//     yang berbeda, jadi highlight dan copy salah. Seleksi kini
//     disimpan dalam koordinat absolut (lihat selectionAbsLine),
//     yang stabil saat buffer men-scroll: saat satu baris live pindah
//     ke scrollback, scrollbackLen bertambah 1 dan indeks absolut
//     baris itu tetap sama.
package main

// selectionAbsLine mengubah baris tampilan (viewY, 0..Rows-1, sesuai
// LinesForView dengan scrollOffset saat ini) menjadi indeks baris
// absolut: 0 = baris scrollback tertua, scrollbackLen+Rows-1 = baris
// live terbawah. Di alt screen tidak ada scrollback, jadi absolut =
// tampilan. Hasil di-clamp ke total baris yang ada.
func selectionAbsLine(scrollbackLen, rows, scrollOffset int, altActive bool, viewY int) int {
	if altActive {
		if viewY < 0 {
			return 0
		}
		if rows > 0 && viewY >= rows {
			return rows - 1
		}
		return viewY
	}
	abs := scrollbackLen - scrollOffset + viewY
	total := scrollbackLen + rows
	if abs < 0 {
		return 0
	}
	if total > 0 && abs >= total {
		return total - 1
	}
	return abs
}

// selectionViewLine adalah kebalikan selectionAbsLine: mengembalikan
// baris tampilan untuk indeks absolut, dan false kalau baris absolut
// itu sedang tidak terlihat di tampilan saat ini.
func selectionViewLine(scrollbackLen, rows, scrollOffset int, altActive bool, absY int) (int, bool) {
	if altActive {
		if absY < 0 || absY >= rows {
			return 0, false
		}
		return absY, true
	}
	viewY := absY - (scrollbackLen - scrollOffset)
	if viewY < 0 || viewY >= rows {
		return 0, false
	}
	return viewY, true
}

// orderedSelection mengurutkan ujung seleksi (start bisa saja sesudah
// end kalau user drag ke atas / ke kiri) menjadi (y1,x1,y2,x2) dengan
// y1 < y2, atau y1 == y2 dan x1 <= x2.
func orderedSelection(startAbsY, startX, endAbsY, endX int) (y1, x1, y2, x2 int) {
	y1, x1, y2, x2 = startAbsY, startX, endAbsY, endX
	if y1 > y2 || (y1 == y2 && x1 > x2) {
		y1, x1, y2, x2 = y2, x2, y1, x1
	}
	return y1, x1, y2, x2
}

// autoScrollDelta menghitung perubahan scrollOffset per tick/tahap
// auto-scroll saat drag melewati tepi vertikal jendela. mouseClientY
// adalah koordinat Y mouse dalam klien (boleh negatif / melebihi
// tinggi klien, karena drag memakai mouse capture). Kembali: >0 =
// geser tampilan ke atas (masuk scrollback, scrollOffset bertambah),
// <0 = geser ke bawah (kembali ke live, scrollOffset berkurang),
// 0 = mouse di dalam jendela, tidak perlu scroll. Kecepatan diskala
// dengan jarak mouse di luar tepi (1 baris per baris di luar, maks
// 10 baris per tahap) supaya teks yang sangat panjang tetap bisa
// dicapai tanpa menahan mouse terlalu lama, tapi tetap terkendali.
func autoScrollDelta(mouseClientY, clientHeight, charH int) int {
	if charH <= 0 {
		return 0
	}
	if mouseClientY < 0 {
		amount := (-mouseClientY)/charH + 1
		if amount > 10 {
			amount = 10
		}
		return amount
	}
	if mouseClientY >= clientHeight {
		amount := (mouseClientY-clientHeight)/charH + 1
		if amount > 10 {
			amount = 10
		}
		return -amount
	}
	return 0
}
