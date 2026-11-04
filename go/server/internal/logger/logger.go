// Package logger writes activity lines to stdout and to an activity log file,
// and records errors in the error_log table.
package logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
)

const DefaultLogDir = "/var/log/llm-memory-api"

// isoMillis is JavaScript's Date.toISOString format, which the existing log
// lines and the admin UI expect.
const isoMillis = "2006-01-02T15:04:05.000Z"

// Field is one key of a log line's JSON details. Details are an ordered list,
// not a map, so the keys print in the order the caller wrote them.
type Field struct {
	Key   string
	Value any
}

type Fields []Field

func (f Fields) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, field := range f {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := jsonutil.Marshal(field.Key)
		if err != nil {
			return nil, err
		}
		value, err := jsonutil.Marshal(field.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

type Logger struct {
	db     db.DB
	actors *actors.Cache
	stdout io.Writer
	stderr io.Writer
	now    func() time.Time

	// logFile is empty when the log directory is not writable (local dev);
	// lines then go to stdout only.
	logFile string
	fileMu  sync.Mutex
}

// New creates a logger. File logging is on when logDir accepts an append to
// activity.log at startup.
func New(conn db.DB, actorCache *actors.Cache, logDir string) *Logger {
	l := &Logger{
		db:     conn,
		actors: actorCache,
		stdout: os.Stdout,
		stderr: os.Stderr,
		now:    time.Now,
	}
	if logDir != "" {
		path := filepath.Join(logDir, "activity.log")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			l.logFile = path
		}
	}
	return l
}

func (l *Logger) timestamp() string {
	return l.now().UTC().Format(isoMillis)
}

// Log writes "[subsystem] <time> action: {details}" to stdout and
// "<time> [subsystem] action: {details}" to the activity log file.
func (l *Logger) Log(subsystem, action string, details Fields) {
	detailJSON, err := jsonutil.Marshal(details)
	if err != nil {
		detailJSON = []byte(fmt.Sprintf("%q", err.Error()))
	}
	fmt.Fprintf(l.stdout, "[%s] %s %s: %s\n", subsystem, l.timestamp(), action, detailJSON)

	if l.logFile == "" {
		return
	}
	line := fmt.Sprintf("%s [%s] %s: %s\n", l.timestamp(), subsystem, action, detailJSON)
	// Opened per line, as the Node server did with fs.appendFile, so log
	// rotation that moves the file away is picked up without a reopen signal.
	// Written synchronously under the lock so lines keep their order.
	l.fileMu.Lock()
	defer l.fileMu.Unlock()
	f, err := os.OpenFile(l.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(l.stderr, "Failed to write to %s: %s\n", l.logFile, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		fmt.Fprintf(l.stderr, "Failed to write to %s: %s\n", l.logFile, err)
	}
}

// Errorf writes a line to stderr, like console.error in the Node server.
func (l *Logger) Errorf(format string, args ...any) {
	fmt.Fprintf(l.stderr, format+"\n", args...)
}

// ErrorOpts describes one error for LogError. Empty strings and a zero
// StatusCode are stored as NULL.
type ErrorOpts struct {
	Agent      string
	Context    string
	ContextID  string
	Message    string
	Detail     string
	StatusCode int
}

// LogError writes the error to the activity log and inserts an error_log row.
// The insert runs in the background and never fails the caller.
func (l *Logger) LogError(subsystem, action string, opts ErrorOpts) {
	var details Fields
	if opts.Agent != "" {
		details = append(details, Field{"agent", opts.Agent})
	}
	if opts.Context != "" {
		details = append(details, Field{"context", opts.Context})
	}
	if opts.ContextID != "" {
		details = append(details, Field{"contextId", opts.ContextID})
	}
	details = append(details, Field{"error", opts.Message})
	l.Log(subsystem, action, details)

	db.Background(func(ctx context.Context) {
		var actorID *int
		if opts.Agent != "" && l.actors != nil {
			actor, ok, err := l.actors.ResolveByName(ctx, opts.Agent)
			if err != nil {
				l.Errorf("Failed to write to error_log table: %s", err)
				return
			}
			if ok {
				actorID = &actor.ID
			}
		}
		_, err := l.db.Exec(ctx,
			`INSERT INTO error_log (subsystem, action, actor_id, context, context_id, error_message, error_detail, status_code)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			subsystem, action, actorID, nullable(opts.Context), nullable(opts.ContextID),
			opts.Message, nullable(opts.Detail), nullableInt(opts.StatusCode))
		if err != nil {
			l.Errorf("Failed to write to error_log table: %s", err)
		}
	})
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
