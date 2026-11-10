package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteJoinParentBase(t *testing.T) {
	if got := remoteJoin("/", "file.txt"); got != "/file.txt" {
		t.Fatalf("remoteJoin / = %q", got)
	}
	if got := remoteJoin("/root/toko", "a.zip"); got != "/root/toko/a.zip" {
		t.Fatalf("remoteJoin = %q", got)
	}
	if got := remoteParent("/root/toko"); got != "/root" {
		t.Fatalf("remoteParent = %q", got)
	}
	if got := remoteParent("/"); got != "/" {
		t.Fatalf("remoteParent / = %q", got)
	}
	if got := remoteBase("/root/toko/a.zip"); got != "a.zip" {
		t.Fatalf("remoteBase = %q", got)
	}
}

func TestResolvePromptDir(t *testing.T) {
	if got := resolvePromptDir("~", "/root"); got != "/root" {
		t.Fatalf("~ = %q", got)
	}
	if got := resolvePromptDir("~/toko", "/root"); got != "/root/toko" {
		t.Fatalf("~/toko = %q", got)
	}
	if got := resolvePromptDir("/var/www", "/root"); got != "/var/www" {
		t.Fatalf("abs = %q", got)
	}
	if got := resolvePromptDir("", "/home/user"); got != "/home/user" {
		t.Fatalf("empty = %q", got)
	}
}

func TestFormatSizeEntry(t *testing.T) {
	if got := formatSize(500); got != "500 B" {
		t.Fatalf("500 = %q", got)
	}
	if got := formatSize(1536); got != "1.5 KB" {
		t.Fatalf("1536 = %q", got)
	}
	e := SFTPEntry{Name: "notes", Size: 2048, ModTime: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	if got := formatEntry(e); got != "notes  (2.0 KB, 2026-10-30 12:00)" {
		t.Fatalf("formatEntry = %q", got)
	}
	if got := formatEntry(SFTPEntry{Name: "toko", IsDir: true}); got != "toko/" {
		t.Fatalf("dir = %q", got)
	}
}

func TestSortEntriesDirsFirst(t *testing.T) {
	es := []SFTPEntry{{Name: "z.txt"}, {Name: "a"}, {Name: "b", IsDir: true}, {Name: "A", IsDir: true}}
	sortEntries(es)
	if es[0].Name != "A" || es[1].Name != "b" || es[2].Name != "a" || es[3].Name != "z.txt" {
		t.Fatalf("order = %+v", es)
	}
}

func TestEntryColumns(t *testing.T) {
	d := SFTPEntry{Name: "toko", IsDir: true, ModTime: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	if got := entryType(d); got != "File folder" {
		t.Fatalf("dir type = %q", got)
	}
	if got := entrySizeText(d); got != "" {
		t.Fatalf("dir size = %q", got)
	}
	if got := entryChangedText(d); got != "19/09/2026 12:00" {
		t.Fatalf("dir changed = %q", got)
	}
	f := SFTPEntry{Name: "a.txt", Size: 2048, ModTime: time.Date(2026, 10, 30, 8, 30, 0, 0, time.UTC)}
	if got := entryType(f); got != "File" {
		t.Fatalf("file type = %q", got)
	}
	if got := entrySizeText(f); got != "2.0 KB" {
		t.Fatalf("file size = %q", got)
	}
}

func TestWithParentEntryAndValidateName(t *testing.T) {
	es := withParentEntry([]SFTPEntry{{Name: "b.txt"}, {Name: "a", IsDir: true}})
	if len(es) != 3 || !es[0].IsParent || es[0].Name != ".." {
		t.Fatalf("withParentEntry = %+v", es)
	}
	if got := entryType(es[0]); got != "Parent directory" {
		t.Fatalf("parent type = %q", got)
	}
	if err := validateNewName(""); err == nil {
		t.Fatalf("empty name should fail")
	}
	if err := validateNewName(".."); err == nil {
		t.Fatalf(".. should fail")
	}
	if err := validateNewName("a/b"); err == nil {
		t.Fatalf("a/b should fail")
	}
	if err := validateNewName("new_folder-1"); err != nil {
		t.Fatalf("valid name failed: %v", err)
	}
}

func TestFormatPickLabel(t *testing.T) {
	if got := formatPickLabel("Localhost", "root@127.0.0.1", "/home/root"); got != "Localhost — root@127.0.0.1 — /home/root" {
		t.Fatalf("pick label = %q", got)
	}
	if got := formatPickLabel("", "root@127.0.0.1", ""); got != "root@127.0.0.1" {
		t.Fatalf("pick label = %q", got)
	}
	if got := formatPickLabel("root@127.0.0.1", "root@127.0.0.1", ""); got != "root@127.0.0.1" {
		t.Fatalf("duplicate name/who should not repeat, got %q", got)
	}
}

func TestEditBinaryAndTempPath(t *testing.T) {
	if !isKnownBinaryExt(".zip") || !isKnownBinaryExt("exe") || isKnownBinaryExt(".json") || isKnownBinaryExt(".txt") || isKnownBinaryExt("") {
		t.Fatalf("binary ext classification wrong")
	}
	if !isBinaryContent([]byte("ab\x00cd")) || isBinaryContent([]byte("{\"a\": 1}\n")) {
		t.Fatalf("binary content sniff wrong")
	}
	got := editTempPath("/tmp", "root@127.0.0.1", "/home/root/config.json")
	want := filepath.Join("/tmp", "KERM-SFTP", "root_127.0.0.1", "home", "root", "config.json")
	if got != want {
		t.Fatalf("edit temp path = %q, want %q", got, want)
	}
}

func TestListLocal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	es, err := ListLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || !es[0].IsDir || es[0].Name != "sub" || es[1].Name != "b.txt" {
		t.Fatalf("ListLocal = %+v", es)
	}
}

func TestProgressWriterCancel(t *testing.T) {
	var buf bytes.Buffer
	cancel := &atomic.Bool{}
	cancel.Store(true)
	pw := &progressWriter{w: &buf, total: 10, cancel: cancel, onProgress: func(_, _ int64) {}}
	if _, err := pw.Write([]byte("x")); !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got %v", err)
	}
	var sent int64
	cancel.Store(false)
	pw2 := &progressWriter{w: &buf, total: 10, cancel: cancel, onProgress: func(s, _ int64) { sent = s }}
	if _, err := pw2.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if sent != 5 || buf.String() != "hello" {
		t.Fatalf("sent=%d buf=%q", sent, buf.String())
	}
}
