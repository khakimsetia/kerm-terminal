package main

import "testing"

func TestShouldSkipTermResize(t *testing.T) {
	if !shouldSkipTermResize(1) {
		t.Fatalf("SIZE_MINIMIZED (1) harus dilewati")
	}
	for _, w := range []uintptr{0, 2, 3, 4} {
		if shouldSkipTermResize(w) {
			t.Fatalf("WM_SIZE wParam=%d tidak boleh dilewati", w)
		}
	}
}

func TestTermResizeSameSizePreservesContent(t *testing.T) {
	// Restore ke ukuran yang sama dengan sebelum minimize harus
	// no-op: Resize ke ukuran yang sama kembali lebih awal dan
	// tidak memotong isi apa pun. Ini yang membuat perbaikan minimize
	// bekerja: buffer tidak pernah menyusut saat minimize, jadi
	// restore ke ukuran semula tidak mengubah buffer sama sekali.
	term := NewTerm(10, 4)
	term.Feed([]byte("line1\r\nline2\r\nline3"))
	before := text(term)
	term.Resize(10, 4)
	after := text(term)
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("resize same-size changed row %d: %q -> %q", i, before[i], after[i])
		}
	}
}
