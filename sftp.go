// SFTP v1 untuk KERM (2026-10-30, atas permintaan user).
//
// Perintah dasar saja, sesuai desain yang disepakati:
//   - list directory (lokal + remote),
//   - upload file lokal -> server,
//   - download file server -> lokal,
//   - cek timpa sebelum menulis (tidak pernah timpa diam-diam).
//
// Bukan fitur: transfer direktori rekursif, rename/delete/mkdir/chmod,
// resume. Itu tambahan nanti kalau diminta.
//
// Arsitektur: mesin ini murni Go, tanpa Win32, supaya bisa di-test di
// Linux. UI Win32 (sftp_ui.go) hanya memanggil fungsi di sini dan
// menampilkan hasilnya. Koneksi SFTP memakai ssh.Client terminal yang
// SAMA (sftp.NewClient(conn.Client)), jadi tidak perlu login ulang,
// tidak perlu verifikasi host key ulang, dan shell tetap jalan saat
// transfer berjalan atau dibatalkan. SFTP tidak berlaku untuk Telnet:
// Telnet tidak punya channel/subsystem SSH sama sekali.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
)

// ErrExists: file tujuan sudah ada. UI harus bertanya ke user
// (Overwrite / Skip / Cancel) sebelum memanggil lagi dengan
// overwrite=true. Mesin tidak pernah timpa diam-diam.
var ErrExists = errors.New("file already exists")

// ErrCancelled: transfer dibatalkan user (tombol Cancel).
var ErrCancelled = errors.New("transfer cancelled")

// SFTPEntry: satu baris di daftar file, lokal maupun remote.
// Diurutkan: direktori dulu, lalu file, masing-masing alfabetis.
// IsParent: entry virtual ".." di atas list (bukan file nyata) untuk
// kembali ke folder sebelumnya, seperti WinSCP.
type SFTPEntry struct {
	Name     string
	Size     int64
	ModTime  time.Time
	IsDir    bool
	IsParent bool
}

// withParentEntry: sisipkan ".." di paling atas list (setelah
// diurutkan). Dipakai UI lokal + remote, supaya selalu ada jalan
// kembali tanpa tombol Up saja.
func withParentEntry(entries []SFTPEntry) []SFTPEntry {
	out := make([]SFTPEntry, 0, len(entries)+1)
	out = append(out, SFTPEntry{Name: "..", IsDir: true, IsParent: true})
	out = append(out, entries...)
	return out
}

// validateNewName: nama file/folder baru untuk Rename / New folder.
// Tolak kosong, ".", "..", dan nama yang mengandung separator path
// ("/", "\") — nama itu satu segmen saja, bukan path.
func validateNewName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name is empty")
	}
	if name == "." || name == ".." {
		return errors.New("name cannot be . or ..")
	}
	if strings.ContainsAny(name, "/\\") {
		return errors.New("name cannot contain / or \\")
	}
	if strings.ContainsRune(name, 0) {
		return errors.New("name cannot contain NUL")
	}
	return nil
}

// sortEntries: direktori dulu, lalu nama alfabetis (case-insensitive).
func sortEntries(es []SFTPEntry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir
		}
		return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name)
	})
}

// remoteJoin: gabung direktori remote + nama file. Path remote SELALU
// pakai "/", bukan separator Windows, jadi pakai path (bukan filepath).
func remoteJoin(dir, name string) string {
	if dir == "" {
		return name
	}
	return path.Join(dir, name)
}

// remoteParent: direktori induk remote. "/" tetap "/".
func remoteParent(dir string) string {
	if dir == "" || dir == "/" {
		return "/"
	}
	return path.Dir(dir)
}

// remoteBase: nama file saja dari path remote.
func remoteBase(p string) string {
	return path.Base(p)
}

// resolvePromptDir: terjemahkan direktori prompt shell ("~", "~/x",
// "/absolut") menjadi path remote absolut, memakai home absolut dari
// SFTP (Getwd). Prompt "~" TIDAK boleh dikirim mentah ke SFTP: server
// OpenSSH tidak meng-expand "~" di path SFTP, jadi "~" harus jadi
// home, "~/x" jadi home+"/x".
func resolvePromptDir(promptDir, home string) string {
	if promptDir == "" {
		return home
	}
	if promptDir == "~" {
		return home
	}
	if strings.HasPrefix(promptDir, "~/") {
		if home == "" {
			return strings.TrimPrefix(promptDir, "~/")
		}
		return remoteJoin(home, strings.TrimPrefix(promptDir, "~/"))
	}
	return promptDir
}

// formatSize: ukuran manusiawi untuk daftar file.
func formatSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	}
	if size < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(size)/1024/1024)
	}
	return fmt.Sprintf("%.1f GB", float64(size)/1024/1024/1024)
}

// formatEntry: satu baris teks (dipakai tes + fallback). UI SFTP
// WinSCP-like memakai kolom terpisah di bawah, bukan teks gabungan.
func formatEntry(e SFTPEntry) string {
	if e.IsDir {
		return e.Name + "/"
	}
	return fmt.Sprintf("%s  (%s, %s)", e.Name, formatSize(e.Size), e.ModTime.Format("2006-01-02 15:04"))
}

// Kolom ListView gaya WinSCP (screenshot referensi user 2026-10-30):
// Name | Size | Type | Changed. Direktori: Size kosong (seperti
// WinSCP), Type "File folder". Tanggal format DD/MM/YYYY seperti
// locale Windows user, bukan YYYY-MM-DD.
func entryType(e SFTPEntry) string {
	if e.IsParent {
		return "Parent directory"
	}
	if e.IsDir {
		return "File folder"
	}
	return "File"
}

func entrySizeText(e SFTPEntry) string {
	if e.IsDir {
		return ""
	}
	return formatSize(e.Size)
}

// formatPickLabel: label satu baris untuk satu terminal di pemilih
// SFTP panel. Nama sesi kalau ada, user@host, dan direktori prompt
// saat ini kalau ada — direktori inilah yang membedakan dua terminal
// ke server yang sama tapi sudah cd ke folder berbeda.
func formatPickLabel(sessionName, who, promptDir string) string {
	label := who
	if sessionName != "" && sessionName != who {
		label = sessionName + " — " + who
	}
	if promptDir != "" {
		label += " — " + promptDir
	}
	return label
}

// isKnownBinaryExt: ekstensi yang jelas-jelas biner — Enter pada
// file ini langsung download, tanpa percobaan Edit (supaya file besar
// tidak di-download dua kali: sekali ke temp untuk sniff, sekali ke
// lokal). Daftar ini sengaja konservatif: ekstensi yang tidak ada di
// sini tetap dicoba Edit, lalu diputuskan dari isi file (lihat
// isBinaryContent), jadi file teks dengan ekstensi aneh tetap bisa
// diedit.
func isKnownBinaryExt(ext string) bool {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "exe", "dll", "so", "dylib", "zip", "tar", "gz", "bz2", "xz", "7z", "rar",
		"png", "jpg", "jpeg", "gif", "bmp", "ico", "webp", "pdf",
		"doc", "docx", "xls", "xlsx", "ppt", "pptx",
		"mp3", "mp4", "avi", "mov", "mkv", "wav", "flac",
		"iso", "img", "deb", "rpm", "apk", "bin", "db", "sqlite", "sqlite3":
		return true
	}
	return false
}

// isBinaryContent: true kalau data mengandung byte NUL — penanda
// paling andal untuk file biner vs teks (file teks tidak pernah
// mengandung NUL). Dipakai setelah download ke temp, sebelum editor
// dibuka, supaya file biner tanpa ekstensi / ekstensi aneh tidak
// dibuka di Notepad dan rusak karena tersimpan.
func isBinaryContent(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// sanitizeWho: user@host untuk nama folder temp — hanya huruf,
// angka, titik, dan strip yang dipertahankan, sisanya jadi _.
func sanitizeWho(who string) string {
	var sb strings.Builder
	for _, r := range who {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "server"
	}
	return sb.String()
}

// editTempPath: path file sementara untuk Edit, di bawah baseTemp
// (biasanya %TEMP%). Struktur meniru remote path, supaya dua file
// dengan nama sama di folder / server berbeda tidak tabrakan, dan
// ekstensi dipertahankan (penting untuk syntax highlighting di
// Notepad++). Contoh: <base>/KERM-SFTP/root_127.0.0.1/home/root/config.json
func editTempPath(baseTemp, who, remotePath string) string {
	rel := strings.TrimPrefix(remotePath, "/")
	rel = strings.ReplaceAll(rel, "/", string(filepath.Separator))
	return filepath.Join(baseTemp, "KERM-SFTP", sanitizeWho(who), rel)
}

func entryChangedText(e SFTPEntry) string {
	if e.IsParent || e.ModTime.IsZero() {
		return ""
	}
	return e.ModTime.Format("02/01/2006 15:04")
}

// ListLocal: daftar isi direktori lokal, sudah diurutkan.
func ListLocal(dir string) ([]SFTPEntry, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]SFTPEntry, 0, len(ents))
	for _, ent := range ents {
		info, err := ent.Info()
		if err != nil {
			continue
		}
		out = append(out, SFTPEntry{Name: ent.Name(), Size: info.Size(), ModTime: info.ModTime(), IsDir: ent.IsDir()})
	}
	sortEntries(out)
	return out, nil
}

// ListRemote: daftar isi direktori remote via SFTP, sudah diurutkan.
func ListRemote(client *sftp.Client, dir string) ([]SFTPEntry, error) {
	infos, err := client.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]SFTPEntry, 0, len(infos))
	for _, info := range infos {
		out = append(out, SFTPEntry{Name: info.Name(), Size: info.Size(), ModTime: info.ModTime(), IsDir: info.IsDir()})
	}
	sortEntries(out)
	return out, nil
}

// ProgressFunc: dipanggil selama transfer, dari goroutine transfer.
// UI harus meng-throttle pengiriman ke UI thread (pesan WM_APP) supaya
// antrean pesan tidak banjir untuk file besar.
type ProgressFunc func(sent, total int64)

// progressWriter: menghitung byte yang lewat dan memanggil onProgress.
// Kalau cancel di-set, Write gagal dengan ErrCancelled sehingga
// io.Copy berhenti dan file parsial bisa dihapus pemanggil.
type progressWriter struct {
	w          io.Writer
	sent       int64
	total      int64
	onProgress ProgressFunc
	cancel     *atomic.Bool
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if p.cancel != nil && p.cancel.Load() {
		return 0, ErrCancelled
	}
	n, err := p.w.Write(b)
	if n > 0 {
		p.sent += int64(n)
		if p.onProgress != nil {
			p.onProgress(p.sent, p.total)
		}
	}
	return n, err
}

// UploadFile: lokal -> remote. overwrite=false + remote sudah ada =
// ErrExists (UI bertanya dulu, lalu panggil lagi dengan true).
// Kalau gagal / dibatalkan di tengah jalan, file remote parsial
// DIHAPUS, supaya tidak dikira file lengkap.
func UploadFile(client *sftp.Client, localPath, remotePath string, overwrite bool, cancel *atomic.Bool, onProgress ProgressFunc) error {
	if !overwrite {
		if _, err := client.Stat(remotePath); err == nil {
			return ErrExists
		}
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("directories are not supported in SFTP v1, please select a file")
	}
	dst, err := client.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	pw := &progressWriter{w: dst, total: info.Size(), onProgress: onProgress, cancel: cancel}
	_, copyErr := io.Copy(pw, src)
	closeErr := dst.Close()
	if copyErr != nil {
		// File parsial jangan dibiarkan: hapus, lalu laporkan
		// penyebab aslinya (cancel atau error transfer).
		_ = client.Remove(remotePath)
		return copyErr
	}
	return closeErr
}

// DownloadFile: remote -> lokal. overwrite=false + lokal sudah ada =
// ErrExists. File lokal parsial dihapus kalau gagal / dibatalkan.
func DownloadFile(client *sftp.Client, remotePath, localPath string, overwrite bool, cancel *atomic.Bool, onProgress ProgressFunc) error {
	if !overwrite {
		if _, err := os.Stat(localPath); err == nil {
			return ErrExists
		}
	}
	src, err := client.Open(remotePath)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("directories are not supported in SFTP v1, please select a file")
	}
	dst, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	pw := &progressWriter{w: dst, total: info.Size(), onProgress: onProgress, cancel: cancel}
	_, copyErr := io.Copy(pw, src)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(localPath)
		return copyErr
	}
	return closeErr
}

// localJoin: gabung direktori lokal + nama (separator Windows di PC).
func localJoin(dir, name string) string {
	return filepath.Join(dir, name)
}

// localParent: direktori induk lokal.
func localParent(dir string) string {
	return filepath.Dir(dir)
}
