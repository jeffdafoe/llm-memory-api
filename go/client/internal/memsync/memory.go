// Package memsync implements the three sync phases: memory file sync,
// conversation log upload, and note directory sync. Each phase
// communicates with the memory API via the api.Client.
package memsync

import (
    "crypto/md5"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "time"

    "github.com/jeffdafoe/llm-memory-api/go/client/internal/api"
)

// MemorySync runs Phase 1: bidirectional sync of memory/*.md files.
// Sends the local file list to the server, which returns pull/push/unchanged
// actions. Pulls overwrite local files; pushes update remote. Timestamps
// are aligned after each operation so future syncs see them as equal.
// Returns the conversation retention_days from the server response
// (used by Phase 2 to decide whether to sync conversations).
//
// pruneRemote makes the local directory authoritative for existence: a remote
// note with no local file is soft-deleted rather than pulled back down, so a
// memory consolidation that retires files propagates without a manual
// delete_note per slug. The server performs the deletion (it owns the
// namespace and slug prefix) and reports it back as a "prune" action.
func MemorySyncWithConvConfig(client *api.Client, projectDir string, pruneRemote bool) (int, error) {
    memoryDir := filepath.Join(projectDir, "memory")

    // Ensure memory directory exists
    if err := os.MkdirAll(memoryDir, 0755); err != nil {
        return 0, fmt.Errorf("create memory dir: %w", err)
    }

    // Marked before the scan, not after: the server treats a remote note
    // updated later than this as a concurrent session's creation and pulls it
    // instead of pruning. Taking the mark first keeps that window conservative.
    // We send the server how long ago this was rather than when it was — the
    // elapsed time is the gap between two readings of one clock (monotonic, so
    // it also survives a wall-clock adjustment mid-run), which lets the server
    // place the cutoff on its own clock instead of trusting ours.
    scanStart := time.Now()

    // Scan local .md files
    entries, err := os.ReadDir(memoryDir)
    if err != nil {
        return 0, fmt.Errorf("read memory dir: %w", err)
    }

    var localFiles []memoryFile
    for _, entry := range entries {
        if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
            continue
        }
        filePath := filepath.Join(memoryDir, entry.Name())
        info, err := entry.Info()
        if err != nil {
            continue
        }
        content, err := os.ReadFile(filePath)
        if err != nil {
            continue
        }
        localFiles = append(localFiles, memoryFile{
            Filename: entry.Name(),
            Content:  string(content),
            Mtime:    info.ModTime().UTC().Format(time.RFC3339Nano),
        })
    }

    // Call sync endpoint
    var result memorySyncResponse
    memory := memoryPayload{Files: localFiles}
    if pruneRemote {
        scanAgeMs := time.Since(scanStart).Milliseconds()
        memory.Prune = true
        memory.ScanAgeMs = &scanAgeMs
    }
    err = client.Post("/agent/memory/sync", memorySyncRequest{
        Memory:        memory,
        Conversations: map[string]interface{}{},
    }, &result)
    if err != nil {
        return 0, fmt.Errorf("memory sync request: %w", err)
    }

    // Process actions
    pulled := 0
    pushed := 0
    unchanged := 0
    skipped := 0
    pruned := 0

    for _, action := range result.Memory.Actions {
        // Prune is settled server-side and touches nothing locally, so it is
        // handled before the filename guard — that guard exists to stop a
        // server-supplied name from steering a local write, and there is no
        // write here. Checking it first would report an already-completed
        // deletion as "skipped" for any slug the server derives to a non-.md
        // filename.
        if action.Action == "prune" {
            fmt.Printf("  PRUNE (remote deleted) %s\n", action.Filename)
            pruned++
            continue
        }

        if !isSafeFilename(action.Filename) || !strings.HasSuffix(action.Filename, ".md") {
            fmt.Fprintf(os.Stderr, "  SKIP unsafe filename from server: %s\n", action.Filename)
            skipped++
            continue
        }

        filePath := filepath.Join(memoryDir, action.Filename)

        switch action.Action {
        case "pull":
            if err := os.WriteFile(filePath, []byte(action.Content), 0644); err != nil {
                fmt.Fprintf(os.Stderr, "  PULL ERROR %s: %s\n", action.Filename, err)
                continue
            }
            if action.RemoteUpdatedAt != "" {
                setMtime(filePath, action.RemoteUpdatedAt)
            }
            fmt.Printf("  PULL %s\n", action.Filename)
            pulled++

        case "push":
            if action.RemoteUpdatedAt != "" {
                setMtime(filePath, action.RemoteUpdatedAt)
            }
            fmt.Printf("  PUSH %s\n", action.Filename)
            pushed++

        default:
            unchanged++
        }
    }

    summary := fmt.Sprintf("Memory sync complete: %d pulled, %d pushed, %d unchanged", pulled, pushed, unchanged)
    if pruned > 0 {
        summary += fmt.Sprintf(", %d pruned", pruned)
    }
    if skipped > 0 {
        summary += fmt.Sprintf(", %d skipped (unsafe filenames)", skipped)
    }
    fmt.Println(summary)

    // Return conversation retention config for Phase 2
    retentionDays := 0
    if result.Conversations != nil {
        retentionDays = result.Conversations.RetentionDays
    }

    return retentionDays, nil
}

// --- Types for the /agent/memory/sync endpoint ---

type memoryFile struct {
    Filename string `json:"filename"`
    Content  string `json:"content"`
    Mtime    string `json:"mtime"`
}

type memoryPayload struct {
    Files []memoryFile `json:"files"`
    // Omitted entirely unless --prune-remote is set, so an older server that
    // doesn't know the field sees exactly the request it saw before.
    Prune bool `json:"prune,omitempty"`
    // Milliseconds between the local directory scan and this request. A
    // pointer, not a plain int64: a scan fast enough to round to 0 ms is the
    // ordinary case, and omitempty on a value type would drop exactly that
    // field and fail the server's prune validation. Nil omits it entirely,
    // which is what an unflagged run needs.
    ScanAgeMs *int64 `json:"scan_age_ms,omitempty"`
}

type memorySyncRequest struct {
    Memory        memoryPayload          `json:"memory"`
    Conversations map[string]interface{} `json:"conversations"`
}

type memoryAction struct {
    Filename        string `json:"filename"`
    Action          string `json:"action"`
    Content         string `json:"content,omitempty"`
    RemoteUpdatedAt string `json:"remote_updated_at,omitempty"`
}

type memorySyncResponse struct {
    Memory struct {
        Actions []memoryAction `json:"actions"`
    } `json:"memory"`
    Conversations *conversationConfig `json:"conversations,omitempty"`
}

type conversationConfig struct {
    RetentionDays int      `json:"retention_days,omitempty"`
    Missing       []string `json:"missing,omitempty"`
    Stale         []string `json:"stale,omitempty"`
    Uploaded      int      `json:"uploaded,omitempty"`
    UploadErrors  []struct {
        SessionID string `json:"session_id"`
        Error     string `json:"error"`
    } `json:"upload_errors,omitempty"`
}

// --- Helpers ---

// isSafeFilename rejects filenames that could escape the local directory.
// Must be a flat basename: no slashes, no traversal, no leading dots.
func isSafeFilename(name string) bool {
    if name == "" {
        return false
    }
    if strings.Contains(name, "/") || strings.Contains(name, "\\") {
        return false
    }
    if name == "." || name == ".." {
        return false
    }
    if strings.HasPrefix(name, ".") {
        return false
    }
    return true
}

// setMtime parses an ISO timestamp and sets the file's modification time.
func setMtime(filePath string, timestamp string) {
    t, err := time.Parse(time.RFC3339Nano, timestamp)
    if err != nil {
        // Try alternate formats the server might return
        t, err = time.Parse("2006-01-02T15:04:05.000Z", timestamp)
        if err != nil {
            return
        }
    }
    _ = os.Chtimes(filePath, t, t)
}

// MD5Hash returns the hex MD5 digest of a string, matching Postgres MD5(content).
func MD5Hash(s string) string {
    h := md5.Sum([]byte(s))
    return fmt.Sprintf("%x", h)
}
