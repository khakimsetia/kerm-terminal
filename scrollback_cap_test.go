package main

import (
	"fmt"
	"testing"
)

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
