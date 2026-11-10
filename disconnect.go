// Disconnect reason mapping, pure Go (no Win32) so it can be unit-tested
// on Linux. Used by wmAppClosed in main_windows.go.
//
// User request 2026-10-29 (clarified same day): the full disconnect
// notice (banner in the terminal + MessageBox) is restored to the
// 3-line form below — that part was already good:
//
//	Disconnected from root@host
//	Network error: Connection Timeout
//	Time: 2026-10-29 20:44:00
//
// Only the status box below the Connect button changes: it is a log,
// not a single replaced line. Each event appends one short line
// ("Connected to root@host", then "Disconnected from root@host",
// ...), nothing is deleted, and the view scrolls to the latest line
// (see appendStatus in main_windows.go). So the status box keeps the
// short one-line form per event, while the banner / MessageBox keep
// the full 3-line form with network error and time.
//
// Clean vs abnormal, from Session.Wait() (SSH) / read error (Telnet):
//   - err == nil: clean exit/logout. Old behaviour kept: terminal
//     closes, panel reopens.
//   - err != nil: abnormal disconnect (network, sleep, server restart,
//     reset, timeout, etc.). Terminal stays open with the 3-line notice.
// Exception: "use of closed network connection" happens when we close
// the connection ourselves (closing a terminal) — not a server
// disconnect, so it is treated as clean to avoid a false dialog.
package main

import "strings"

// disconnectInfo is the mapped disconnect reason. clean=true means a
// clean exit.
type disconnectInfo struct {
	clean  bool
	reason string
	detail string
}

func disconnectReason(err error) disconnectInfo {
	if err == nil {
		return disconnectInfo{clean: true, reason: "Session Ended"}
	}
	detail := err.Error()
	low := strings.ToLower(detail)
	if strings.Contains(low, "use of closed network connection") {
		return disconnectInfo{clean: true, reason: "Connection Closed", detail: detail}
	}
	reason := "Connection Lost"
	switch {
	case strings.Contains(low, "timeout") || strings.Contains(low, "i/o timeout"):
		reason = "Connection Timeout"
	case strings.Contains(low, "connection reset") || strings.Contains(low, "broken pipe"):
		reason = "Connection Reset"
	case strings.Contains(low, "network is unreachable") || strings.Contains(low, "no route to host"):
		reason = "Network Unreachable"
	case strings.Contains(low, "connection refused"):
		reason = "Connection Refused"
	case strings.Contains(low, "disconnect"):
		reason = "Disconnected by Server"
	case strings.Contains(low, "eof"):
		reason = "Connection Closed by Server"
	}
	return disconnectInfo{clean: false, reason: reason, detail: detail}
}

// disconnectNotice builds the full 3-line disconnect notice for the
// terminal banner and the MessageBox. who is "user@host" (SSH) or
// "host" (Telnet), timeStr is already formatted, e.g.
// "2026-10-29 20:44:00".
func disconnectNotice(who, reason, timeStr string) string {
	return "Disconnected from " + who + "\r\n" +
		"Network error: " + reason + "\r\n" +
		"Time: " + timeStr
}

// disconnectLine builds the one short line used per event in the
// status-box log: "Disconnected from root@host".
func disconnectLine(who string) string {
	return "Disconnected from " + who
}

// connectFailureReason: judul singkat + detail singkat untuk popup
// gagal connect (2026-11-10, atas permintaan user, lalu disederhanakan
// 2026-11-10: popupnya simpel saja, bukan penjelasan panjang + petunjuk
// + detail error asli). Bentuk popup jadinya:
//   Judul: Connection Timeout
//   Isi:   Error connect SSH failed: host:port i/o timeout
// notify=false untuk kasus di mana user BARU SAJA menjawab dialog:
// host key tidak dikonfirmasi / host key berubah lalu user klik No —
// menampilkan popup kedua setelah dialog itu hanya mengganggu, status
// log saja sudah cukup. Untuk semua kegagalan lain (timeout, refused,
// DNS, auth, dll) notify=true.
// Murni Go, bisa dites di Linux seperti disconnectReason.
func connectFailureReason(err error) (title, detail string, notify bool) {
	if err == nil {
		return "Connection Failed", "failed", false
	}
	low := strings.ToLower(err.Error())
	// User sudah menjawab dialog host key — jangan popup lagi.
	if strings.Contains(low, "host key not confirmed") {
		return "Host Key Not Confirmed", "", false
	}
	if strings.Contains(low, "host key changed") {
		return "Host Key Changed", "", false
	}
	switch {
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded"):
		return "Connection Timeout", "i/o timeout", true
	case strings.Contains(low, "connection refused") || strings.Contains(low, "no connection could be made"):
		return "Connection Refused", "connection refused", true
	case strings.Contains(low, "no such host") || strings.Contains(low, "name or service not known") || strings.Contains(low, "temporary failure in name resolution"):
		return "Host Not Found", "no such host", true
	case strings.Contains(low, "network is unreachable") || strings.Contains(low, "no route to host"):
		return "Network Unreachable", "network is unreachable", true
	case strings.Contains(low, "unable to authenticate") || strings.Contains(low, "no supported methods remain") || strings.Contains(low, "handshake failed"):
		return "Authentication Failed", "unable to authenticate", true
	case strings.Contains(low, "failed to read .ppk") || strings.Contains(low, "failed to read key"):
		return "Key Error", "failed to read key", true
	case strings.Contains(low, "connection reset"):
		return "Connection Reset", "connection reset", true
	default:
		return "Connection Failed", err.Error(), true
	}
}
