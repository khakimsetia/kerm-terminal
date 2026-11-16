package main

import (
	"fmt"
	"testing"
)

// TestThumbPosLargeScrollback: regresi bug 2026-11-13 — drag thumb
// scrollbar lompat ke paling atas begitu scrollback melewati 32767
// baris. Penyebabnya: posisi thumb dibaca sebagai int16 dari high
// word wParam, jadi 40000 terbungkus jadi -25536 dan offset
// di-clamp ke maksimum (= paling atas). scrollOffsetFromThumbPos
// menerima posisi 32-bit, jadi 40000 tetap 40000.
func TestThumbPosLargeScrollback(t *testing.T) {
	cases := []struct{ sb, thumbPos, want int }{
		{40000, 40000, 0},     // thumb di bawah = live
		{40000, 0, 40000},     // thumb di atas = baris paling atas
		{40000, 20000, 20000}, // thumb di tengah
		{100000, 40000, 60000},
		{100000, 99999, 1},
		{100, 200, 0}, // thumbPos di luar batas, clamp ke live
		{100, -5, 100}, // thumbPos negatif, clamp ke atas
	}
	for _, c := range cases {
		if got := scrollOffsetFromThumbPos(c.sb, c.thumbPos); got != c.want {
			t.Fatalf("scrollOffsetFromThumbPos(%d, %d) = %d, want %d", c.sb, c.thumbPos, got, c.want)
		}
	}
	// Bukti bug lama: int16 dari 40000 = -25536, dan sb-(-25536)
	// akan di-clamp ke sb (= paling atas) — persis gejala yang
	// dilaporkan. (40000 harus lewat variabel, karena konstanta
	// 40000 tidak bisa di-convert ke int16.)
	pos := 40000
	if got := int16(pos); got != -25536 {
		t.Fatalf("int16(40000) = %d, want -25536 (bukti pembungkus 16-bit)", got)
	}
}

// TestLinesForViewNoCopy: regresi bug 2026-11-13 — jeda saat cat
// file panjang. LinesForView dulu menyalin SELURUH scrollback di
// setiap repaint (O(scrollback)); sekarang harus mengembalikan
// referensi ke baris yang sama (O(Rows)), tanpa menyalin.
func TestLinesForViewNoCopy(t *testing.T) {
	term := NewTerm(10, 3)
	for i := 1; i <= 50000; i++ {
		term.Feed([]byte(fmt.Sprintf("line%d\r\n", i)))
	}
	if got := term.ScrollbackLen(); got != 49998 {
		t.Fatalf("scrollback = %d, want 49998", got)
	}
	top := term.LinesForView(term.ScrollbackLen())
	if got := term.LineText(top, 0); got != "line1" {
		t.Fatalf("top row0 = %q, want line1", got)
	}
	// Baris yang dikembalikan harus baris yang sama di scrollback,
	// bukan salinan: alamat sel pertamanya harus sama.
	if len(top) == 0 || len(top[0]) == 0 || len(term.scrollback) == 0 || len(term.scrollback[0]) == 0 {
		t.Fatalf("baris kosong, tidak bisa membandingkan alamat")
	}
	if &top[0][0] != &term.scrollback[0][0] {
		t.Fatalf("LinesForView menyalin baris (alamat berbeda), harusnya referensi")
	}
	// Dan baris tengah tampilan juga harus referensi, bukan salinan.
	mid := term.LinesForView(100)
	if &mid[0][0] != &term.scrollback[term.ScrollbackLen()-100][0] {
		t.Fatalf("LinesForView(100) menyalin baris, harusnya referensi")
	}
}

// TestScrollbarRangeScaled: pemetaan scrollbar (2026-11-13, perbaikan
// "scrollbar selalu kembali ke atas"). Untuk sb <= 65535 pemetaan
// persis; untuk sb yang lebih besar jarak gerak diskalakan ke 65535
// supaya posisi thumb 16-bit di wParam tetap bermakna. Ujung-ujung
// harus persis: thumb 0 = paling atas, thumb maksimum = live.
func TestScrollbarRangeScaled(t *testing.T) {
	// Langsung (sb kecil): travelMax = sb, page = rows.
	if tm, pg := scrollbarRange(100, 30); tm != 100 || pg != 30 {
		t.Fatalf("scrollbarRange(100,30) = (%d,%d), want (100,30)", tm, pg)
	}
	if tm, pg := scrollbarRange(65535, 30); tm != 65535 || pg != 30 {
		t.Fatalf("scrollbarRange(65535,30) = (%d,%d), want (65535,30)", tm, pg)
	}
	// Diskalakan (sb besar): travelMax = 65535, page mengecil.
	if tm, pg := scrollbarRange(100000, 30); tm != 65535 || pg != 19 {
		t.Fatalf("scrollbarRange(100000,30) = (%d,%d), want (65535,19)", tm, pg)
	}
	// Posisi thumb -> posisi scroll, ujung-ujung persis.
	if got := scrollThumbPosToPos(100, 30, 0); got != 0 {
		t.Fatalf("thumb 0 (sb kecil) = %d, want 0 (atas)", got)
	}
	if got := scrollThumbPosToPos(100, 30, 100); got != 100 {
		t.Fatalf("thumb 100 (sb kecil) = %d, want 100 (live)", got)
	}
	if got := scrollThumbPosToPos(100000, 30, 0); got != 0 {
		t.Fatalf("thumb 0 (sb besar) = %d, want 0 (atas)", got)
	}
	if got := scrollThumbPosToPos(100000, 30, 65535); got != 100000 {
		t.Fatalf("thumb 65535 (sb besar) = %d, want 100000 (live)", got)
	}
	// Posisi scroll -> posisi thumb, ujung-ujung persis.
	if got := scrollPosToThumbPos(100000, 30, 0); got != 0 {
		t.Fatalf("pos 0 (sb besar) = %d, want 0", got)
	}
	if got := scrollPosToThumbPos(100000, 30, 100000); got != 65535 {
		t.Fatalf("pos 100000 (sb besar) = %d, want 65535", got)
	}
	// Pulang-pergi (round-trip) tidak boleh meleset jauh: di mode
	// langsung harus persis, di mode skala toleransi satu langkah
	// skala (sb/65535 baris).
	for _, sb := range []int{100, 1000, 65535, 100000, 500000} {
		for _, pos := range []int{0, 1, sb / 2, sb - 1, sb} {
			thumb := scrollPosToThumbPos(sb, 30, pos)
			back := scrollThumbPosToPos(sb, 30, thumb)
			diff := back - pos
			if diff < 0 {
				diff = -diff
			}
			tol := 1
			if sb > 65535 {
				tol = sb/65535 + 1
			}
			if diff > tol {
				t.Fatalf("round-trip sb=%d pos=%d -> thumb=%d -> %d (selisih %d, toleransi %d)", sb, pos, thumb, back, diff, tol)
			}
		}
	}
}

// TestScrollbackUnlimited: scrollback tanpa batas (2026-11-13).
// Dulu dibatasi 2000 baris, output yang panjang sekali kehilangan
// baris paling atas. Sekarang semua baris tersimpan: 12000 baris
// masuk, 11998 di scrollback, dan baris paling atas yang bisa
// dijangkau (LinesForView di offset maksimum) adalah line1.
func TestScrollbackUnlimited(t *testing.T) {
	term := NewTerm(10, 3)
	for i := 1; i <= 12000; i++ {
		term.Feed([]byte(fmt.Sprintf("line%d\r\n", i)))
	}
	if got := term.ScrollbackLen(); got != 11998 {
		t.Fatalf("scrollback = %d, want 11998", got)
	}
	top := term.LinesForView(term.ScrollbackLen())
	if got := term.LineText(top, 0); got != "line1" {
		t.Fatalf("top row0 = %q, want line1", got)
	}
}
