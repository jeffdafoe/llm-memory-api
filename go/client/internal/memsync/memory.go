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

    "github.com/bill1st/llm-memory-api/go/client/internal/api"
)

// MemorySync runs Phase 1: bidirectional sync of memory/*.md files.
// Sends the local file list to the server, which returns pull/push/unchanged
// actions. Pulls overwrite local files; pushes update remote. Timestamps
// are aligned after each operation so future syncs see them as equal.
// Returns the conversation retention_days from the server response
// (used by Phase 2 to decide whether to sync conversations).
func MemorySyncWithConvConfig(client *api.Client, projectDir string) (int, error) {
    memoryDir := filepath.Join(projectDir, "memory")

    // Ensure memory directory exists
    if err := os.MkdirAll(memoryDir, 0755); err != nil {
        return 0, fmt.Errorf("create memory dir: %w", err)
    }

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
    err = client.Post("/agent/memory/sync", memorySyncRequest{
        Memory:        memoryPayload{Files: localFiles},
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

    for _, action := range result.Memory.Actions {
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
