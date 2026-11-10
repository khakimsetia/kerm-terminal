//go:build windows

// Edit file server di editor teks (2026-10-30, atas permintaan user).
// Enter / double-click pada file remote:
//   - file biner yang sudah dikenal (zip, exe, png, ...) -> download
//     biasa, tanpa percobaan Edit.
//   - file lain -> download ke file sementara di %TEMP%\KERM-SFTP\,
//     cek isi: kalau mengandung byte NUL = biner -> hapus temp, log,
//     user pakai tombol Download kalau memang mau mengunduhnya.
//   - kalau teks -> buka di editor (Notepad++ kalau ketemu, kalau
//     tidak Notepad bawaan), lalu pantau file sementara itu.
// Setiap Save di editor -> upload balik ke path remote yang sama
// (menimpa, tanpa konfirmasi lagi — Save itu sendiri niatnya).
//
// Kenapa dipantau, bukan ditunggu proses editornya: Notepad++ itu
// single-instance — kalau sudah jalan, proses baru langsung keluar
// dan melempar ke jendela yang ada, jadi menunggu proses selesai
// tidak akan pernah jalan. Pemantauan waktu modifikasi + ukuran
// bekerja untuk Notepad maupun Notepad++, apa pun instance-nya.
// Upload hanya dilakukan kalau file stabil selama satu poll (1 detik)
// — supaya tidak mengupload file yang masih ditulis editor.
// Kalau upload gagal (mis. jaringan putus), watcher TIDAK menandai
// file sebagai sudah ter-upload, jadi poll berikutnya mencoba lagi;
// log gagal hanya sekali per rangkaian gagal, supaya log tidak banjir.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/lxn/win"
	"github.com/pkg/sftp"
)

type editSession struct {
	remotePath string
	tempPath   string
	lastMod    time.Time
	lastSize   int64
	// opened: true setelah download + sniff selesai dan editor
	// dibuka. Sebelum itu, sesi ini adalah placeholder yang mencegah
	// Enter kedua membuat download kedua ke temp yang sama.
	opened bool
	// dirty: file temp berubah tapi belum berhasil di-upload balik.
	// uploadFailed: upload terakhir gagal (dirty tetap true, supaya
	// watcher mencoba lagi). Keduanya atomic karena ditulis watcher
	// goroutine dan dibaca UI thread saat jendela SFTP ditutup.
	// PENTING 2026-10-30: peringatan tutup SFTP DULU memakai
	// "ada sesi Edit" saja, padahal sesi itu tetap ada di map
	// walaupun editor sudah ditutup dan upload sudah finish —
	// KERM tidak bisa tahu tab Notepad++ sudah ditutup atau belum
	// (single-instance, tidak memberi tahu KERM). Hasilnya false
	// alarm seperti yang dilaporkan user. Sekarang peringatan hanya
	// kalau dirty / uploadFailed, alias benar-benar ada perubahan
	// yang belum sampai ke server.
	dirty        atomic.Bool
	uploadFailed atomic.Bool
}

// findTextEditor: Notepad++ dulu (PATH, lalu Program Files), kalau
// tidak ada, Notepad bawaan Windows (selalu ada). Mengembalikan
// path editor dan nama tampilannya untuk log.
func findTextEditor() (string, string) {
	if p, err := exec.LookPath("notepad++.exe"); err == nil {
		return p, "Notepad++"
	}
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, "Notepad++", "notepad++.exe")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, "Notepad++"
		}
	}
	return "notepad.exe", "Notepad"
}

// startEdit: mulai Edit untuk satu file remote. Dipanggil dari UI
// thread (handler double-click / Enter). Download ke temp dan buka
// editor jalan di goroutine, supaya UI tidak freeze untuk file besar.
func (w *sftpWin) startEdit(remotePath, name string) {
	if !w.ready || w.client == nil || w.transferInProg {
		return
	}
	w.editsMu.Lock()
	if w.edits == nil {
		w.edits = map[string]*editSession{}
	}
	if es, ok := w.edits[remotePath]; ok {
		w.editsMu.Unlock()
		if !es.opened {
			w.appendLog("Still downloading for edit: " + name + " (please wait)")
			return
		}
		// Sudah dibuka: fokuskan lagi editor yang sama (tidak ada
		// temp kedua, tidak ada watcher kedua).
		editor, editorName := findTextEditor()
		win.ShellExecute(w.hwnd, utf16("open"), utf16(editor), utf16(`"`+es.tempPath+`"`), nil, win.SW_SHOWNORMAL)
		w.appendLog("Already open in " + editorName + ": " + name)
		return
	}
	// Placeholder langsung di UI thread, supaya Enter kedua yang
	// datang selagi download berjalan tidak membuat download kedua.
	es := &editSession{remotePath: remotePath, tempPath: editTempPath(os.TempDir(), termWho(w.term), remotePath)}
	w.edits[remotePath] = es
	w.editsMu.Unlock()

	hwnd := w.hwnd
	client := w.client
	tempPath := es.tempPath
	// Hapus placeholder kalau gagal / ternyata biner, supaya Enter
	// berikutnya bisa mencoba lagi dari awal.
	fail := func(op, errMsg string, err error) {
		w.editsMu.Lock()
		delete(w.edits, remotePath)
		w.editsMu.Unlock()
		_ = errMsg
		postMessage(hwnd, wmAppSFTPOpDone, storeSFTPOp(&sftpOpResult{op: op, name: name, err: err}), 0)
	}
	w.appendLog("Editing: " + name + " (downloading to temp file)")
	go func() {
		if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
			fail("edit", "", err)
			return
		}
		if err := DownloadFile(client, remotePath, tempPath, true, nil, nil); err != nil {
			fail("edit", "", err)
			return
		}
		// Sniff biner: baca 8 KB pertama, cari NUL.
		head := make([]byte, 8192)
		f, err := os.Open(tempPath)
		if err != nil {
			fail("edit", "", err)
			return
		}
		n, _ := f.Read(head)
		f.Close()
		if isBinaryContent(head[:n]) {
			_ = os.Remove(tempPath)
			w.editsMu.Lock()
			delete(w.edits, remotePath)
			w.editsMu.Unlock()
			// Bukan lewat MessageBox dari goroutine: kirim pesan
			// op dengan err khusus, handler UI akan menampilkan
			// pesan yang jelas (lihat wmAppSFTPOpDone untuk "edit").
			postMessage(hwnd, wmAppSFTPOpDone, storeSFTPOp(&sftpOpResult{op: "edit-binary", name: name, err: nil}), 0)
			return
		}
		info, err := os.Stat(tempPath)
		if err != nil {
			fail("edit", "", err)
			return
		}
		es.lastMod = info.ModTime()
		es.lastSize = info.Size()
		es.opened = true
		editor, editorName := findTextEditor()
		win.ShellExecute(hwnd, utf16("open"), utf16(editor), utf16(`"`+tempPath+`"`), nil, win.SW_SHOWNORMAL)
		postMessage(hwnd, wmAppSFTPOpDone, storeSFTPOp(&sftpOpResult{op: "edit-open", name: name + " in " + editorName, err: nil}), 0)
		go w.watchEdit(es, client)
	}()
}

// watchEdit: poll file temp tiap 1 detik. Kalau ModTime/ukuran berubah
// dan stabil selama satu poll, upload balik ke server. Berhenti
// kalau jendela SFTP ditutup (w.closed), file temp hilang, atau
// sesi dihapus dari map.
func (w *sftpWin) watchEdit(es *editSession, client *sftp.Client) {
	var pendingMod time.Time
	var pendingSize int64
	var pending bool
	failedLogged := false
	for {
		time.Sleep(time.Second)
		if w.closed.Load() {
			return
		}
		w.editsMu.Lock()
		_, still := w.edits[es.remotePath]
		w.editsMu.Unlock()
		if !still {
			return
		}
		info, err := os.Stat(es.tempPath)
		if err != nil {
			// Temp hilang (dihapus manual / dibersihkan): berhenti,
			// hapus sesi supaya Enter berikutnya memulai dari awal.
			w.editsMu.Lock()
			delete(w.edits, es.remotePath)
			w.editsMu.Unlock()
			return
		}
		if info.ModTime().Equal(es.lastMod) && info.Size() == es.lastSize {
			pending = false
			continue
		}
		// Ada perubahan yang belum ter-upload.
		es.dirty.Store(true)
		if !pending || !info.ModTime().Equal(pendingMod) || info.Size() != pendingSize {
			// Perubahan pertama kali terlihat: tunggu satu poll
			// lagi untuk memastikan file sudah stabil (editor
			// selesai menulis), bukan setengah tertulis.
			pending = true
			pendingMod = info.ModTime()
			pendingSize = info.Size()
			continue
		}
		// Stabil: upload balik.
		pending = false
		if err := UploadFile(client, es.tempPath, es.remotePath, true, nil, nil); err != nil {
			es.uploadFailed.Store(true)
			if !failedLogged {
				postMessage(w.hwnd, wmAppSFTPOpDone, storeSFTPOp(&sftpOpResult{op: "upload", name: filepath.Base(es.remotePath), err: err}), 0)
				failedLogged = true
			}
			// lastMod/lastSize TIDAK diperbarui: poll berikutnya
			// mencoba lagi (remote file mungkin terhapus parsial
			// oleh UploadFile saat gagal — retry akan membuatnya
			// lagi dari temp yang utuh).
			continue
		}
		failedLogged = false
		es.uploadFailed.Store(false)
		es.dirty.Store(false)
		es.lastMod = pendingMod
		es.lastSize = pendingSize
		postMessage(w.hwnd, wmAppSFTPOpDone, storeSFTPOp(&sftpOpResult{op: "upload", name: filepath.Base(es.remotePath), err: nil}), 0)
	}
}

// editPendingPaths: path temp dari sesi Edit yang benar-benar punya
// perubahan belum ter-upload (dirty / upload gagal). Dipakai
// peringatan tutup SFTP — sesi yang sudah upload finish TIDAK masuk
// sini, jadi tidak ada lagi false alarm seperti laporan user.
func (w *sftpWin) editPendingPaths() []string {
	w.editsMu.Lock()
	defer w.editsMu.Unlock()
	var out []string
	for _, es := range w.edits {
		if es.dirty.Load() || es.uploadFailed.Load() {
			out = append(out, es.tempPath)
		}
	}
	return out
}
