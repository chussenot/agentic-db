package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mrzor/claude-status/internal/db"
)

// maxLogBytes caps the hook error log. When the file would exceed this, it is
// rotated (the current log moved to <log>.1, replacing any prior rotation) so
// the active file never grows unbounded. ~256KB keeps a useful tail of recent
// failures without leaking disk.
const maxLogBytes = 256 * 1024

// logFileName is the basename of the error log, written into the state dir (the
// directory containing the DB).
const logFileName = "hook.log"

// logError appends a timestamped line describing err to the ring-buffered hook
// log in the same directory as dbPath. It is best-effort: any failure to log is
// silently ignored (the hook must still exit 0). It is the only writer of the
// log, so it never races itself within a single hook invocation.
func logError(dbPath string, err error) {
	if err == nil {
		return
	}
	logPath := filepath.Join(filepath.Dir(dbPath), logFileName)

	// Rotate if the existing file is already at/over the cap, so the active log
	// stays bounded.
	if fi, statErr := os.Stat(logPath); statErr == nil && fi.Size() >= maxLogBytes {
		// Replace the previous rotation with the current log; ignore failures.
		_ = os.Rename(logPath, logPath+".1")
	}

	f, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		return
	}
	defer f.Close()

	ts := db.Now().UTC().Format(time.RFC3339Nano)
	fmt.Fprintf(f, "%s %v\n", ts, err)
}
