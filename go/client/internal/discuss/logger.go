package discuss

import (
	"fmt"
	"os"
	"time"
)

// Logger writes timestamped messages to both stderr and a log file.
type Logger struct {
	logFile string
}

// NewLogger creates a logger that writes to the given file path.
// The file is created/appended to as messages are logged.
// Pass "" to log only to stderr.
func NewLogger(logFile string) *Logger {
	return &Logger{logFile: logFile}
}

// Log writes a timestamped message to stderr and the log file.
func (l *Logger) Log(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] %s\n", ts, msg)

	fmt.Fprint(os.Stderr, line)

	if l.logFile != "" {
		f, err := os.OpenFile(l.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			f.WriteString(line)
			f.Close()
		}
	}
}

// SetLogFile updates the log file path. Called after workDir is known.
func (l *Logger) SetLogFile(path string) {
	l.logFile = path
}
