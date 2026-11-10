package main

import "testing"

func TestKeyCharToSend(t *testing.T) {
	if got := keyCharToSend(0x08); len(got) != 1 || got[0] != 0x7F {
		t.Fatalf("Backspace (^H) must send DEL, got %v", got)
	}
	if got := keyCharToSend(0x7F); len(got) != 1 || got[0] != 0x7F {
		t.Fatalf("DEL must stay DEL, got %v", got)
	}
	if got := keyCharToSend('a'); string(got) != "a" {
		t.Fatalf("normal char must pass through, got %v", got)
	}
	if got := keyCharToSend('\r'); string(got) != "\r" {
		t.Fatalf("Enter must pass through, got %v", got)
	}
	if got := keyCharToSend(0x03); len(got) != 1 || got[0] != 0x03 {
		t.Fatalf("Ctrl+C must pass through, got %v", got)
	}
	if got := keyCharToSend(0); len(got) != 1 || got[0] != 0 {
		t.Fatalf("Ctrl+Space (NUL) must be sent, got %v", got)
	}
}

func TestModifierCode(t *testing.T) {
	cases := []struct {
		shift, ctrl, alt bool
		want             int
	}{
		{false, false, false, 1},
		{true, false, false, 2},
		{false, false, true, 3},
		{true, false, true, 4},
		{false, true, false, 5},
		{true, true, false, 6},
		{false, true, true, 7},
		{true, true, true, 8},
	}
	for _, c := range cases {
		if got := modifierCode(c.shift, c.ctrl, c.alt); got != c.want {
			t.Fatalf("modifier shift=%v ctrl=%v alt=%v: got %d, want %d", c.shift, c.ctrl, c.alt, got, c.want)
		}
	}
}

func TestKeySeqs(t *testing.T) {
	check := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: got %q, want %q", name, got, want)
		}
	}
	check("arrow plain", string(arrowSeq('A', false, false, false, false)), "\x1b[A")
	check("arrow app", string(arrowSeq('A', true, false, false, false)), "\x1bOA")
	check("arrow ctrl", string(arrowSeq('D', false, false, true, false)), "\x1b[1;5D")
	check("arrow ctrl app", string(arrowSeq('D', true, false, true, false)), "\x1bO1;5D")
	check("arrow shift", string(arrowSeq('B', false, true, false, false)), "\x1b[1;2B")
	check("home plain", string(homeEndSeq('H', false, false, false, false)), "\x1b[H")
	check("home app", string(homeEndSeq('H', true, false, false, false)), "\x1bOH")
	check("end ctrl", string(homeEndSeq('F', false, false, true, false)), "\x1b[1;5F")
	check("delete plain", string(tildeSeq(3, false, false, false)), "\x1b[3~")
	check("delete ctrl", string(tildeSeq(3, false, true, false)), "\x1b[3;5~")
	check("pgup plain", string(tildeSeq(5, false, false, false)), "\x1b[5~")
	check("f1 plain", string(fKeySeq('P', false, false, false)), "\x1bOP")
	check("f1 ctrl", string(fKeySeq('P', false, true, false)), "\x1b[1;5P")
	check("f5 plain", string(tildeSeq(15, false, false, false)), "\x1b[15~")
	check("f6 plain", string(tildeSeq(17, false, false, false)), "\x1b[17~")
	check("f11 plain", string(tildeSeq(23, false, false, false)), "\x1b[23~")
	check("f12 plain", string(tildeSeq(24, false, false, false)), "\x1b[24~")
	check("f5 shift", string(tildeSeq(15, true, false, false)), "\x1b[15;2~")
	check("meta b", string(metaSeq('b')), "\x1b" + "b")
	if metaSeq(0x01) != nil {
		t.Fatalf("meta control must be nil")
	}
	check("shift-tab", string(tabSeq(true)), "\x1b[Z")
	check("tab", string(tabSeq(false)), "\x09")
}

func TestMouseSeq(t *testing.T) {
	check := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: got %q, want %q", name, got, want)
		}
	}
	check("sgr left press", string(mouseSeq(0, 10, 5, true, false)), "\x1b[<0;10;5M")
	check("sgr left release", string(mouseSeq(0, 10, 5, true, true)), "\x1b[<0;10;5m")
	check("sgr wheel up", string(mouseSeq(64, 10, 5, true, false)), "\x1b[<64;10;5M")
	check("sgr motion", string(mouseSeq(32, 10, 5, true, false)), "\x1b[<32;10;5M")
	check("x10 left press", string(mouseSeq(0, 10, 5, false, false)), "\x1b[M *%")
	// X10: button 0+32=' ', x 10+32=42='*', y 5+32=37='%'
	check("x10 release", string(mouseSeq(0, 10, 5, false, true)), "\x1b[M#*%")
	// X10 release: button 3+32="#", x "," , y "%"

	if got := bracketedPasteWrap("hi", true); got != "\x1b[200~hi\x1b[201~" {
		t.Fatalf("bracketed paste: got %q", got)
	}
	if got := bracketedPasteWrap("hi", false); got != "hi" {
		t.Fatalf("unbracketed paste: got %q", got)
	}
}
