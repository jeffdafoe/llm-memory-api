package discuss

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Paths holds all file/directory paths derived from the working directory.
type Paths struct {
	Inbox          string
	Outbox         string
	LogFile        string
	StateFile      string
	DoneFile       string
	TranscriptFile string
	IdleTimeoutFile string
	PromptFile     string
	PollScript     string
	LockDir        string
	LockInfoFile   string
	ResultFile     string
}

// InitPaths creates a Paths struct from the working directory.
func InitPaths(workDir string) *Paths {
	lockDir := filepath.Join(workDir, ".lock")
	return &Paths{
		Inbox:           filepath.Join(workDir, "inbox"),
		Outbox:          filepath.Join(workDir, "outbox"),
		LogFile:         filepath.Join(workDir, "conversation.log"),
		StateFile:       filepath.Join(workDir, "state.json"),
		DoneFile:        filepath.Join(workDir, "done"),
		TranscriptFile:  filepath.Join(workDir, "transcript.md"),
		IdleTimeoutFile: filepath.Join(workDir, "idle-timeout"),
		PromptFile:      filepath.Join(workDir, "prompt.txt"),
		PollScript:      filepath.Join(workDir, "poll.sh"),
		LockDir:         lockDir,
		LockInfoFile:    filepath.Join(lockDir, "info.json"),
		ResultFile:      filepath.Join(workDir, "result.md"),
	}
}

// EnsureDirectories creates the inbox and outbox directories.
func (p *Paths) EnsureDirectories() error {
	if err := os.MkdirAll(p.Inbox, 0755); err != nil {
		return fmt.Errorf("create inbox: %w", err)
	}
	if err := os.MkdirAll(p.Outbox, 0755); err != nil {
		return fmt.Errorf("create outbox: %w", err)
	}
	return nil
}

// CleanWorkDir removes stale files from a previous run. Does NOT touch
// state.json, .lock, or transcript.md — those have their own lifecycle.
func (p *Paths) CleanWorkDir() {
	// Clear inbox and outbox contents
	for _, dir := range []string{p.Inbox, p.Outbox} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}

	// Remove stale control/log files
	staleFiles := []string{"done", "idle-timeout", "subagent.log", "conversation.log"}
	workDir := filepath.Dir(p.LogFile)
	for _, name := range staleFiles {
		os.Remove(filepath.Join(workDir, name))
	}
}

// IsDone returns true if the done file exists.
func (p *Paths) IsDone() bool {
	_, err := os.Stat(p.DoneFile)
	return err == nil
}

// IsIdleTimeout returns true if the idle-timeout file exists.
func (p *Paths) IsIdleTimeout() bool {
	_, err := os.Stat(p.IdleTimeoutFile)
	return err == nil
}

// WriteDone creates the done file with the given reason.
func (p *Paths) WriteDone(reason string) error {
	return os.WriteFile(p.DoneFile, []byte(reason), 0644)
}

// WriteInboxFile writes a message file to the inbox directory.
func (p *Paths) WriteInboxFile(filename, content string) error {
	return os.WriteFile(filepath.Join(p.Inbox, filename), []byte(content), 0644)
}

// ReadInboxFiles returns the list of .txt files in the inbox directory.
func (p *Paths) ReadInboxFiles() []string {
	entries, err := os.ReadDir(p.Inbox)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files
}

// InboxFileExists returns true if a specific inbox file exists.
func (p *Paths) InboxFileExists(filename string) bool {
	_, err := os.Stat(filepath.Join(p.Inbox, filename))
	return err == nil
}

// CheckOutbox reads and removes the first .txt file from the outbox.
// Returns the file content and true if a file was found, or empty string
// and false if the outbox is empty.
func (p *Paths) CheckOutbox() (string, bool) {
	entries, err := os.ReadDir(p.Outbox)
	if err != nil {
		return "", false
	}

	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		path := filepath.Join(p.Outbox, f)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		os.Remove(path)
		msg := string(data)
		if strings.TrimSpace(msg) != "" {
			return msg, true
		}
	}

	return "", false
}

// WritePollScript writes the bash polling script that the subagent
// uses to wait for new inbox messages or control signals.
func (p *Paths) WritePollScript(workDir string) {
	// Use forward slashes for bash compatibility on Windows
	wd := filepath.ToSlash(workDir)
	script := fmt.Sprintf(`#!/bin/bash
# Poll for new inbox messages, done file, or idle timeout.
# Outputs: NEW_MESSAGES, DONE, or IDLE_TIMEOUT
idle_count=0
while true; do
  files=$(ls %s/inbox/*.txt 2>/dev/null)
  if [ -n "$files" ]; then echo "NEW_MESSAGES"; echo "$files"; break; fi
  if [ -f %s/done ]; then echo "DONE"; break; fi
  idle_count=$((idle_count + 1))
  if [ $idle_count -ge 60 ]; then echo "IDLE_TIMEOUT"; break; fi
  sleep 5
done
`, wd, wd)
	os.WriteFile(p.PollScript, []byte(script), 0755)
}

// --- State file ---

// TransportState is the JSON structure persisted to state.json.
type TransportState struct {
	Status          string          `json:"status"`
	PID             int             `json:"pid"`
	Port            int             `json:"port"`
	Heartbeat       string          `json:"heartbeat"`
	TurnCount       int             `json:"turnCount"`
	LastDeliveredID int             `json:"lastDeliveredId"`
	SeenIDs         []int           `json:"seenIds"`
	Ready           bool            `json:"ready"`
	StartedAt       string          `json:"startedAt"`
	Convergence     ConvergenceInfo `json:"convergence"`
}

// ConvergenceInfo is the convergence section of the state file.
type ConvergenceInfo struct {
	State                 string `json:"state"`
	RoundTrips            int    `json:"roundTrips"`
	MaxRounds             int    `json:"maxRounds"`
	ExchangesSinceLastVote int   `json:"exchangesSinceLastVote"`
	RejectedConcludes     int    `json:"rejectedConcludes"`
}

// WriteState atomically writes the state file.
func (p *Paths) WriteState(state *TransportState) {
	state.Heartbeat = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	tmp := p.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	os.Rename(tmp, p.StateFile)
}

// ReadState reads the state file. Returns nil if it doesn't exist or
// can't be parsed.
func (p *Paths) ReadState() *TransportState {
	data, err := os.ReadFile(p.StateFile)
	if err != nil {
		return nil
	}
	var state TransportState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	return &state
}

// --- Lock file ---

// lockInfo is the JSON written inside the lock directory.
type lockInfo struct {
	PID     int   `json:"pid"`
	Started int64 `json:"started"`
}

const lockStaleMs = 30000

// AcquireLock prevents two transport instances from running for the
// same discussion. Uses a PID lockfile inside a lock directory.
func (p *Paths) AcquireLock() error {
	err := os.Mkdir(p.LockDir, 0755)
	if err == nil {
		// Successfully created — write lock info
		return p.writeLockInfo()
	}

	if !os.IsExist(err) {
		return fmt.Errorf("create lock dir: %w", err)
	}

	// Lock dir exists — check if stale via mtime
	info, statErr := os.Stat(p.LockInfoFile)
	if statErr == nil {
		age := time.Since(info.ModTime())
		if age.Milliseconds() < lockStaleMs {
			// Lock is fresh — read PID for error message
			data, _ := os.ReadFile(p.LockInfoFile)
			var li lockInfo
			json.Unmarshal(data, &li)
			return fmt.Errorf("another transport (PID %d) is already running", li.PID)
		}
	}

	// Stale lock — clean up and recreate
	os.Remove(p.LockInfoFile)
	os.Remove(p.LockDir)
	if err := os.Mkdir(p.LockDir, 0755); err != nil {
		return fmt.Errorf("recreate lock dir: %w", err)
	}
	return p.writeLockInfo()
}

func (p *Paths) writeLockInfo() error {
	li := lockInfo{PID: os.Getpid(), Started: time.Now().UnixMilli()}
	data, _ := json.Marshal(li)
	return os.WriteFile(p.LockInfoFile, data, 0644)
}

// TouchLock updates the mtime on the lock info file so it doesn't
// appear stale to other instances.
func (p *Paths) TouchLock() {
	now := time.Now()
	os.Chtimes(p.LockInfoFile, now, now)
}

// ReleaseLock removes the lock directory.
func (p *Paths) ReleaseLock() {
	os.Remove(p.LockInfoFile)
	os.Remove(p.LockDir)
}

// --- Transcript ---

// InitTranscript creates the transcript file with a header.
func (p *Paths) InitTranscript(agent string, others []string) {
	header := fmt.Sprintf("# Discussion: %s <-> %s\n\nStarted: %s\n\n---\n\n",
		agent,
		strings.Join(others, ", "),
		time.Now().UTC().Format("2006-01-02 15:04:05"),
	)
	os.WriteFile(p.TranscriptFile, []byte(header), 0644)
}

// AppendTranscript adds a timestamped speaker entry to the transcript.
func (p *Paths) AppendTranscript(speaker, message string) {
	ts := time.Now().Format("15:04:05")
	entry := fmt.Sprintf("**%s** (%s):\n%s\n\n", speaker, ts, message)
	f, err := os.OpenFile(p.TranscriptFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(entry)
}

// --- Stale PID cleanup ---

// KillStalePID reads a previous state.json and kills the process if
// it's still alive.
func (p *Paths) KillStalePID() {
	prev := p.ReadState()
	if prev == nil || prev.PID == 0 || prev.PID == os.Getpid() {
		return
	}
	proc, err := os.FindProcess(prev.PID)
	if err != nil {
		return
	}
	// Check if alive (signal 0 doesn't kill, just checks)
	if err := proc.Signal(nil); err != nil {
		return // Not running
	}
	proc.Kill()
}

// --- Helpers ---

// MessageSize returns the size of a message in KB.
func MessageSize(msg string) string {
	kb := float64(len(msg)) / 1024.0
	return fmt.Sprintf("%.1f", kb)
}

// ParseInboxID extracts the numeric message ID from an inbox filename
// like "42.txt". Returns -1 if the filename doesn't match.
func ParseInboxID(filename string) int {
	name := strings.TrimSuffix(filename, ".txt")
	id, err := strconv.Atoi(name)
	if err != nil {
		return -1
	}
	return id
}
