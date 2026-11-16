package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestCollectLocalFiles: upload folder rekursif (2026-11-16).
// Struktur dipertahankan, folder kosong ikut dilaporkan supaya
// dibuat di server, file dihitung dengan path remote yang benar.
func TestCollectLocalFiles(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.txt", "a")
	mustWrite("sub/b.txt", "bb")
	mustWrite("sub/deep/c.txt", "ccc")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	items, dirs, err := collectLocalFiles(root, "/srv/site")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	wantRemote := map[string]bool{
		"/srv/site/a.txt": true, "/srv/site/sub/b.txt": true, "/srv/site/sub/deep/c.txt": true,
	}
	for _, it := range items {
		if !wantRemote[it.remotePath] {
			t.Fatalf("remotePath tak terduga: %q", it.remotePath)
		}
		if it.size == 0 {
			t.Fatalf("size 0 untuk %q", it.display)
		}
	}
	// Folder kosong harus ada di daftar dirs (dibuat di server).
	foundEmpty := false
	for _, d := range dirs {
		if d == "/srv/site/empty" {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Fatalf("dirs %v tidak memuat /srv/site/empty", dirs)
	}
	// Folder berisi file saja: 0 item, tapi dir-nya dilaporkan.
	items, dirs, err = collectLocalFiles(filepath.Join(root, "empty"), "/srv/site/empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("folder kosong items = %d, want 0", len(items))
	}
	_ = dirs
}

// TestCollectRemoteFilesWith: download folder rekursif (2026-11-16),
// dengan filesystem remote palsu (readDir disuntik).
func TestCollectRemoteFilesWith(t *testing.T) {
	// Pohon palsu: /srv/{x.txt, sub/{y.txt, empty/}}.
	type node struct {
		name  string
		isDir bool
		size  int64
	}
	tree := map[string][]node{
		"/srv":      {{name: "x.txt", size: 10}, {name: "sub", isDir: true}},
		"/srv/sub":  {{name: "y.txt", size: 20}, {name: "empty", isDir: true}},
		"/srv/sub/empty": {},
	}
	readDir := func(dir string) ([]os.FileInfo, error) {
		nodes, ok := tree[dir]
		if !ok {
			return nil, os.ErrNotExist
		}
		var out []os.FileInfo
		for _, n := range nodes {
			out = append(out, &fakeFileInfo{name: n.name, isDir: n.isDir, size: n.size})
		}
		return out, nil
	}
	items, dirs, err := collectRemoteFilesWith(readDir, "/srv", `C:\dl`)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	wantRemote := map[string]bool{"/srv/x.txt": true, "/srv/sub/y.txt": true}
	for _, it := range items {
		if !wantRemote[it.remotePath] {
			t.Fatalf("remotePath tak terduga: %q", it.remotePath)
		}
		if it.display != "x.txt" && it.display != "sub/y.txt" {
			t.Fatalf("display tak terduga: %q", it.display)
		}
	}
	foundEmpty := false
	for _, d := range dirs {
		if filepath.Base(d) == "empty" {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Fatalf("dirs %v tidak memuat folder empty", dirs)
	}
}

type fakeFileInfo struct {
	name  string
	isDir bool
	size  int64
}

func (f *fakeFileInfo) Name() string      { return f.name }
func (f *fakeFileInfo) Size() int64       { return f.size }
func (f *fakeFileInfo) Mode() os.FileMode { return 0o644 }
func (f *fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f *fakeFileInfo) IsDir() bool       { return f.isDir }
func (f *fakeFileInfo) Sys() any          { return nil }

// TestDeleteConfirmText: dialog konfirmasi hapus (2026-11-16,
// multi-delete). Satu file = detail ukuran/tanggal; banyak file =
// daftar nama max 5 + "+N more".
func TestDeleteConfirmText(t *testing.T) {
	one := deleteConfirmText([]SFTPEntry{{Name: "a.txt", Size: 2048, ModTime: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}}, "/srv")
	if !strings.Contains(one, "Delete this file?") || !strings.Contains(one, "/srv/a.txt") || !strings.Contains(one, "2.0 KB") {
		t.Fatalf("satu file: %q", one)
	}
	folder := deleteConfirmText([]SFTPEntry{{Name: "sub", IsDir: true}}, "/srv")
	if !strings.Contains(folder, "Delete this folder?") || !strings.Contains(folder, "Folder must be empty") {
		t.Fatalf("satu folder: %q", folder)
	}
	many := deleteConfirmText([]SFTPEntry{
		{Name: "a.txt"}, {Name: "b.txt"}, {Name: "c.txt"}, {Name: "d.txt"},
		{Name: "e.txt"}, {Name: "f.txt"}, {Name: "g", IsDir: true},
	}, "/srv")
	if !strings.Contains(many, "Delete 7 items?") || !strings.Contains(many, "a.txt") || !strings.Contains(many, "...and 2 more") {
		t.Fatalf("banyak file: %q", many)
	}
	if strings.Contains(many, "f.txt") || strings.Contains(many, "g/") {
		t.Fatalf("daftar harus max 5 nama, tapi: %q", many)
	}
}

// TestApplyOverwriteDecisions: logika timpa gaya WinSCP (2026-11-16).
// "to All" berlaku untuk semua file berikut; Cancel membatalkan semua.
func TestApplyOverwriteDecisions(t *testing.T) {
	items := []transferItem{
		{display: "a.txt"}, {display: "b.txt"}, {display: "c.txt"}, {display: "d.txt"}, {display: "e.txt"},
	}
	allExist := []bool{true, true, true, true, true}
	// 1) Satu-satu: Yes, No, Yes, No, Yes.
	answers := []overwriteDecision{overwriteYes, overwriteNo, overwriteYes, overwriteNo, overwriteYes}
	i := 0
	kept, ok := applyOverwriteDecisions(items, allExist, func(it transferItem) overwriteDecision {
		a := answers[i]
		i++
		return a
	})
	if !ok || len(kept) != 3 || kept[0].display != "a.txt" || kept[1].display != "c.txt" || kept[2].display != "e.txt" {
		t.Fatalf("satu-satu: ok=%v kept=%v", ok, kept)
	}
	if i != 5 {
		t.Fatalf("ask dipanggil %d kali, want 5", i)
	}
	// 2) Yes to All di file ke-2: file 1 ditanya, sisanya ikut.
	i = 0
	answers = []overwriteDecision{overwriteNo, overwriteYesAll}
	kept, ok = applyOverwriteDecisions(items, allExist, func(it transferItem) overwriteDecision {
		a := answers[i]
		i++
		return a
	})
	if !ok || len(kept) != 4 || kept[0].display != "b.txt" {
		t.Fatalf("yes-to-all: ok=%v kept=%v", ok, kept)
	}
	if i != 2 {
		t.Fatalf("ask dipanggil %d kali, want 2 (sisanya ikut Yes to All)", i)
	}
	// 3) No to All di file pertama + file yang tidak ada tetap jalan.
	mixed := []bool{true, false, true, false, true}
	i = 0
	kept, ok = applyOverwriteDecisions(items, mixed, func(it transferItem) overwriteDecision {
		i++
		return overwriteNoAll
	})
	if !ok || len(kept) != 2 || kept[0].display != "b.txt" || kept[1].display != "d.txt" {
		t.Fatalf("no-to-all: ok=%v kept=%v", ok, kept)
	}
	if i != 1 {
		t.Fatalf("ask dipanggil %d kali, want 1", i)
	}
	// 4) Cancel di file ke-2: batalkan semua (ok=false).
	i = 0
	answers = []overwriteDecision{overwriteYes, overwriteCancel}
	kept, ok = applyOverwriteDecisions(items, allExist, func(it transferItem) overwriteDecision {
		a := answers[i]
		i++
		return a
	})
	if ok || kept != nil {
		t.Fatalf("cancel: ok=%v kept=%v, want ok=false kept=nil", ok, kept)
	}
	// 5) Tidak ada yang sudah ada: ask tidak pernah dipanggil.
	none := []bool{false, false, false, false, false}
	kept, ok = applyOverwriteDecisions(items, none, func(it transferItem) overwriteDecision {
		t.Fatal("ask tidak boleh dipanggil kalau tidak ada file yang sudah ada")
		return overwriteYes
	})
	if !ok || len(kept) != 5 {
		t.Fatalf("tidak ada yang sudah ada: ok=%v kept=%d", ok, len(kept))
	}
}

// TestOverwriteDialogText: teks dialog timpa (ukuran + tanggal kedua sisi).
func TestOverwriteDialogText(t *testing.T) {
	txt := overwriteDialogText("a.txt", "/srv/a.txt", 2048, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), 4096, time.Date(2026, 11, 16, 11, 30, 0, 0, time.UTC))
	if !strings.Contains(txt, "/srv/a.txt") || !strings.Contains(txt, "2.0 KB") || !strings.Contains(txt, "4.0 KB") ||
		!strings.Contains(txt, "2026-11-16 10:00") || !strings.Contains(txt, "2026-11-16 11:30") {
		t.Fatalf("teks dialog: %q", txt)
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
