package memsync

import (
    "bufio"
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "regexp"
    "strings"
    "time"

    "github.com/bill1st/llm-memory-api/go/client/internal/api"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ConversationSync runs Phase 2: upload Claude Code session logs.
// Scans the project directory for .jsonl files, diffs against the server,
// preprocesses missing sessions into markdown, and uploads them one at a time
// to avoid exceeding nginx body size limits.
func ConversationSync(client *api.Client, projectDir string, agentName string, userName string, retentionDays int) error {
    cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
    activeThreshold := 1 * time.Minute

    // Scan for JSONL session files
    entries, err := os.ReadDir(projectDir)
    if err != nil {
        return fmt.Errorf("read project dir: %w", err)
    }

    var candidateIDs []string
    // Map lowercase ID to actual filename for case-insensitive matching later
    fileMap := make(map[string]string)

    for _, entry := range entries {
        if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
            continue
        }
        // Skip subagent sessions
        if strings.HasPrefix(entry.Name(), "agent-") {
            continue
        }

        sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
        if !uuidRegex.MatchString(strings.ToLower(sessionID)) {
            continue
        }

        info, err := entry.Info()
        if err != nil {
            continue
        }

        // Skip files outside retention window
        if info.ModTime().Before(cutoff) {
            continue
        }
        // Skip files modified recently (likely active sessions)
        if time.Since(info.ModTime()) < activeThreshold {
            continue
        }

        lowerID := strings.ToLower(sessionID)
        candidateIDs = append(candidateIDs, lowerID)
        fileMap[lowerID] = entry.Name()
    }

    if len(candidateIDs) == 0 {
        fmt.Println("Conversations: no new sessions")
        return nil
    }

    // Ask server which sessions are missing
    var diffResult struct {
        Conversations *conversationConfig `json:"conversations"`
    }
    err = client.Post("/agent/memory/sync", map[string]interface{}{
        "conversations": map[string]interface{}{
            "session_ids": candidateIDs,
        },
    }, &diffResult)
    if err != nil {
        return fmt.Errorf("conversation diff request: %w", err)
    }

    if diffResult.Conversations == nil {
        fmt.Println("Conversations: all up to date")
        return nil
    }
    missing := diffResult.Conversations.Missing
    if len(missing) == 0 {
        fmt.Println("Conversations: all up to date")
        return nil
    }

    // Preprocess and upload missing sessions one at a time
    uploaded := 0
    convErrors := 0
    serverErrors := 0

    for _, sessionID := range missing {
        // Find the local file
        filename, ok := fileMap[strings.ToLower(sessionID)]
        if !ok {
            fmt.Fprintf(os.Stderr, "  CONV SKIP missing file: %s.jsonl\n", sessionID)
            convErrors++
            continue
        }

        filePath := filepath.Join(projectDir, filename)
        messages, err := preprocessSession(filePath, agentName, userName)
        if err != nil {
            fmt.Fprintf(os.Stderr, "  CONV ERROR %s: %s\n", sessionID, err)
            convErrors++
            continue
        }

        content := formatConversation(sessionID, projectDir, messages)
        if content == "" {
            // Empty session — skip silently
            continue
        }

        // Derive date from first message
        dateStr := "unknown"
        if len(messages) > 0 && !messages[0].Timestamp.IsZero() {
            dateStr = messages[0].Timestamp.Format("2006-01-02")
        }

        // Upload
        upload := conversationUpload{
            SessionID: sessionID,
            Date:      dateStr,
            Content:   content,
            Metadata: conversationMetadata{
                SessionID:    sessionID,
                SessionDate:  dateStr,
                Project:      projectDir,
                Agent:        agentName,
                User:         userName,
                MessageCount: len(messages),
                Source:       "claude-code-jsonl",
            },
        }

        var uploadResult struct {
            Conversations *conversationConfig `json:"conversations"`
        }
        err = client.Post("/agent/memory/sync", map[string]interface{}{
            "conversations": map[string]interface{}{
                "uploads": []conversationUpload{upload},
            },
        }, &uploadResult)
        if err != nil {
            fmt.Fprintf(os.Stderr, "  CONV UPLOAD ERROR %s: %s\n", sessionID, err)
            convErrors++
            continue
        }

        if uploadResult.Conversations != nil && len(uploadResult.Conversations.UploadErrors) > 0 {
            for _, ue := range uploadResult.Conversations.UploadErrors {
                fmt.Fprintf(os.Stderr, "  CONV SERVER ERROR %s: %s\n", ue.SessionID, ue.Error)
            }
            serverErrors += len(uploadResult.Conversations.UploadErrors)
        } else {
            count := 0
            if uploadResult.Conversations != nil {
                count = uploadResult.Conversations.Uploaded
            }
            uploaded += count
            fmt.Printf("  CONV conversations/%s-%s\n", dateStr, sessionID)
        }
    }

    summary := fmt.Sprintf("Conversations: %d uploaded", uploaded)
    if convErrors > 0 {
        summary += fmt.Sprintf(", %d errors", convErrors)
    }
    if serverErrors > 0 {
        summary += fmt.Sprintf(", %d server errors", serverErrors)
    }
    fmt.Println(summary)

    return nil
}

// --- JSONL preprocessing ---

type message struct {
    Timestamp time.Time
    Speaker   string
    Text      string
}

// jsonlEntry represents one line of a Claude Code .jsonl session file.
type jsonlEntry struct {
    Type      string          `json:"type"`
    Timestamp string          `json:"timestamp"`
    Message   json.RawMessage `json:"message"`
}

// userMessage can be a string or an object with role/content.
type assistantMessage struct {
    Content json.RawMessage `json:"content"`
}

type contentBlock struct {
    Type string `json:"type"`
    Text string `json:"text"`
}

// preprocessSession reads a Claude Code JSONL file and extracts user
// and assistant text messages, skipping tool calls and system entries.
func preprocessSession(filePath string, agentName string, userName string) ([]message, error) {
    f, err := os.Open(filePath)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    var messages []message
    reader := bufio.NewReader(f)

    for {
        // ReadBytes has no line-length limit, unlike Scanner
        line, err := reader.ReadBytes('\n')
        if len(line) > 0 {
            // Trim trailing newline/carriage return
            line = bytes.TrimRight(line, "\r\n")

            if len(line) > 0 {
                var entry jsonlEntry
                if jsonErr := json.Unmarshal(line, &entry); jsonErr == nil {
                    ts := parseTimestamp(entry.Timestamp)

                    switch entry.Type {
                    case "user":
                        text := extractUserText(entry.Message)
                        if text != "" {
                            messages = append(messages, message{
                                Timestamp: ts,
                                Speaker:   userName,
                                Text:      text,
                            })
                        }

                    case "assistant":
                        text := extractAssistantText(entry.Message)
                        if text != "" {
                            messages = append(messages, message{
                                Timestamp: ts,
                                Speaker:   agentName,
                                Text:      text,
                            })
                        }
                    }
                    // Skip progress, system, queue-operation entries
                }
                // Skip malformed JSON lines silently
            }
        }
        if err != nil {
            if err == io.EOF {
                break
            }
            return nil, err
        }
    }

    return messages, nil
}

// extractUserText pulls text content from a user message.
// The message field can be a string, or an object with a content field
// that is itself a string or an array of content blocks.
func extractUserText(raw json.RawMessage) string {
    if len(raw) == 0 {
        return ""
    }

    // Try as a plain string first
    var s string
    if err := json.Unmarshal(raw, &s); err == nil {
        return strings.TrimSpace(s)
    }

    // Try as an object with content
    var msg struct {
        Content json.RawMessage `json:"content"`
    }
    if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Content) == 0 {
        return ""
    }

    // Content can be a string
    if err := json.Unmarshal(msg.Content, &s); err == nil {
        return strings.TrimSpace(s)
    }

    // Content can be an array of blocks — filter to text only
    var blocks []contentBlock
    if err := json.Unmarshal(msg.Content, &blocks); err != nil {
        return ""
    }

    var parts []string
    for _, b := range blocks {
        if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
            parts = append(parts, b.Text)
        }
    }
    return strings.TrimSpace(strings.Join(parts, "\n"))
}

// extractAssistantText pulls text content from an assistant message,
// filtering out tool_use blocks.
func extractAssistantText(raw json.RawMessage) string {
    if len(raw) == 0 {
        return ""
    }

    var msg assistantMessage
    if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Content) == 0 {
        return ""
    }

    var blocks []contentBlock
    if err := json.Unmarshal(msg.Content, &blocks); err != nil {
        return ""
    }

    var parts []string
    for _, b := range blocks {
        if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
            parts = append(parts, strings.TrimSpace(b.Text))
        }
    }
    return strings.Join(parts, "\n")
}

// formatConversation builds a markdown note from preprocessed messages.
func formatConversation(sessionID string, projectDir string, messages []message) string {
    if len(messages) == 0 {
        return ""
    }

    // Derive date from first message
    dateStr := "unknown"
    if !messages[0].Timestamp.IsZero() {
        dateStr = messages[0].Timestamp.Format("2006-01-02")
    }

    var b strings.Builder
    fmt.Fprintf(&b, "Session: %s (%s)\n", dateStr, sessionID)
    fmt.Fprintf(&b, "Project: %s\n", projectDir)
    b.WriteString("\n---\n\n")

    for i, msg := range messages {
        if i > 0 {
            b.WriteString("\n")
        }
        timeStr := ""
        if !msg.Timestamp.IsZero() {
            timeStr = msg.Timestamp.Format("15:04")
        }
        fmt.Fprintf(&b, "[%s %s] %s\n", timeStr, msg.Speaker, msg.Text)
    }

    return b.String()
}

// parseTimestamp tries to parse an ISO 8601 timestamp string.
func parseTimestamp(s string) time.Time {
    if s == "" {
        return time.Time{}
    }
    // Try common formats
    formats := []string{
        time.RFC3339Nano,
        time.RFC3339,
        "2006-01-02T15:04:05.000Z",
        "2006-01-02T15:04:05Z",
    }
    for _, f := range formats {
        if t, err := time.Parse(f, s); err == nil {
            return t
        }
    }
    return time.Time{}
}

// --- Types for conversation upload ---

type conversationMetadata struct {
    SessionID    string `json:"session_id"`
    SessionDate  string `json:"session_date"`
    Project      string `json:"project"`
    Agent        string `json:"agent"`
    User         string `json:"user"`
    MessageCount int    `json:"message_count"`
    Source       string `json:"source"`
}

type conversationUpload struct {
    SessionID string               `json:"session_id"`
    Date      string               `json:"date"`
    Content   string               `json:"content"`
    Metadata  conversationMetadata `json:"metadata"`
}
