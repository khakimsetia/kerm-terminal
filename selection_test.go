package main

import "testing"

func TestSelectionAbsLine(t *testing.T) {
	// scrollback 100, 10 baris live, tampilan live (offset 0):
	// view 0 = absolut 100, view 9 = absolut 109.
	if got := selectionAbsLine(100, 10, 0, false, 0); got != 100 {
		t.Fatalf("live view0 = %d, want 100", got)
	}
	if got := selectionAbsLine(100, 10, 0, false, 9); got != 109 {
		t.Fatalf("live view9 = %d, want 109", got)
	}
	// Tampilan bergeser 20 ke atas (offset 20): view 0 = absolut 80.
	if got := selectionAbsLine(100, 10, 20, false, 0); got != 80 {
		t.Fatalf("scrolled view0 = %d, want 80", got)
	}
	if got := selectionAbsLine(100, 10, 20, false, 9); got != 89 {
		t.Fatalf("scrolled view9 = %d, want 89", got)
	}
	// Alt screen: absolut = tampilan, tanpa scrollback.
	if got := selectionAbsLine(0, 10, 0, true, 5); got != 5 {
		t.Fatalf("alt view5 = %d, want 5", got)
	}
	// Clamp: view di luar jangkauan tidak boleh keluar total baris.
	if got := selectionAbsLine(100, 10, 0, false, 50); got != 109 {
		t.Fatalf("clamped = %d, want 109", got)
	}
}

func TestSelectionViewLine(t *testing.T) {
	if v, ok := selectionViewLine(100, 10, 20, false, 80); !ok || v != 0 {
		t.Fatalf("abs80 -> view %d,%v, want 0,true", v, ok)
	}
	if _, ok := selectionViewLine(100, 10, 20, false, 79); ok {
		t.Fatalf("abs79 harus di luar tampilan offset 20")
	}
	if _, ok := selectionViewLine(100, 10, 20, false, 90); ok {
		t.Fatalf("abs90 harus di luar tampilan offset 20 (view 0..9 = abs 80..89)")
	}
}

func TestOrderedSelection(t *testing.T) {
	y1, x1, y2, x2 := orderedSelection(10, 5, 5, 2)
	if y1 != 5 || x1 != 2 || y2 != 10 || x2 != 5 {
		t.Fatalf("drag ke atas: got %d,%d-%d,%d", y1, x1, y2, x2)
	}
	y1, x1, y2, x2 = orderedSelection(5, 10, 5, 2)
	if y1 != 5 || x1 != 2 || y2 != 5 || x2 != 10 {
		t.Fatalf("drag ke kiri satu baris: got %d,%d-%d,%d", y1, x1, y2, x2)
	}
}

func TestAutoScrollDelta(t *testing.T) {
	if d := autoScrollDelta(300, 600, 20); d != 0 {
		t.Fatalf("di dalam jendela: %d, want 0", d)
	}
	if d := autoScrollDelta(-10, 600, 20); d != 1 {
		t.Fatalf("sedikit di atas: %d, want +1", d)
	}
	if d := autoScrollDelta(-50, 600, 20); d != 3 {
		t.Fatalf("jauh di atas: %d, want +3", d)
	}
	if d := autoScrollDelta(650, 600, 20); d != -3 {
		t.Fatalf("di bawah: %d, want -3", d)
	}
	if d := autoScrollDelta(1000, 600, 20); d != -10 {
		t.Fatalf("sangat jauh di bawah harus dibatasi: %d, want -10", d)
	}
}

func TestTermLineTextAbs(t *testing.T) {
	term := NewTerm(10, 3)
	term.Feed([]byte("line1\r\nline2\r\nline3\r\nline4"))
	if term.ScrollbackLen() != 1 {
		t.Fatalf("scrollback = %d, want 1", term.ScrollbackLen())
	}
	if got := term.LineTextAbs(0); got != "line1" {
		t.Fatalf("abs0 = %q, want line1", got)
	}
	if got := term.LineTextAbs(1); got != "line2" {
		t.Fatalf("abs1 = %q, want line2", got)
	}
	if got := term.LineTextAbs(3); got != "line4" {
		t.Fatalf("abs3 = %q, want line4", got)
	}
	if got := term.LineTextAbs(4); got != "" {
		t.Fatalf("abs4 di luar jangkauan = %q, want empty", got)
	}
}
