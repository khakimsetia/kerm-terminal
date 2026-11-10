package main

import "testing"

func TestPromptSpans(t *testing.T) {
	cases := []struct {
		line       string
		uh, dir    int
		ok         bool
	}{
		{"root@myazure:~# ", 13, 14, true},
		{"root@myazure:~#", 13, 14, true},
		{"root@myazure:~# ls -la", 13, 14, true},
		{"root@myazure:/home/user$ ", 13, 23, true},
		{"user@host:~$ ", 10, 11, true},
		{"hello world", 0, 0, false},
		{"root@myazure: SSH terhubung", 0, 0, false},
		{"[root@myazure ~]# ", 0, 0, false},
		{"root@myazure:~", 0, 0, false},
	}
	for _, c := range cases {
		uh, dir, ok := promptSpans([]rune(c.line))
		if ok != c.ok || uh != c.uh || dir != c.dir {
			t.Fatalf("promptSpans(%q) = %d,%d,%v want %d,%d,%v", c.line, uh, dir, ok, c.uh, c.dir, c.ok)
		}
	}
}

func TestFeedRecolorsPrompt(t *testing.T) {
	term := NewTerm(80, 24)
	term.PromptRecolor = true
	term.Feed([]byte("root@myazure:~# "))
	row := term.buf[term.CursorY]
	if row[0].Fg != 0x55FF55 || !row[0].Bold || row[0].FgDefault {
		t.Fatalf("user@host not green bold: %+v", row[0])
	}
	if row[13].Fg != 0x5555FF || !row[13].Bold {
		t.Fatalf("dir not blue bold: %+v", row[13])
	}
	if row[14].FgDefault == false {
		t.Fatalf("# should stay default, got %+v", row[14])
	}
	// Tanpa PromptRecolor, prompt tetap default (abu-abu).
	term2 := NewTerm(80, 24)
	term2.Feed([]byte("root@myazure:~# "))
	if term2.buf[term2.CursorY][0].FgDefault == false {
		t.Fatalf("without recolor, prompt should stay default")
	}
}

func TestPromptDir(t *testing.T) {
	term := NewTerm(80, 24)
	term.PromptRecolor = true
	term.Feed([]byte("root@server:/var/www# "))
	if got := term.PromptDir(); got != "/var/www" {
		t.Fatalf("PromptDir = %q", got)
	}
	term2 := NewTerm(80, 24)
	term2.Feed([]byte("hello world\r\n"))
	if got := term2.PromptDir(); got != "" {
		t.Fatalf("non-prompt PromptDir = %q", got)
	}
}
