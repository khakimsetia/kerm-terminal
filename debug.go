// Debug log — pure Go, no Windows dependency.
// Every lifecycle event is appended to a file in the OS temp dir, so if
// the app hangs on Windows the user can send the last lines and we can
// see exactly where it stopped. Logging is best-effort and never blocks
// the UI for long (open-append-close per line).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var logMu sync.Mutex

func debugLogPath() string {
	return filepath.Join(os.TempDir(), "ssh-terminal-debug.log")
}

func logDebug(msg string) {
	logMu.Lock()
	defer logMu.Unlock()
	f, err := os.OpenFile(debugLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	_ = f.Close()
}
