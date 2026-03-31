package memsync

import (
    "fmt"
    "os"
    "path/filepath"
    "regexp"
    "strings"
    "time"

    "github.com/jeffdafoe/llm-memory-api/go/client/internal/api"
)

// NoteSync runs Phase 3: sync configured note slug/prefix mappings to
// local directories. Fetches mappings from the server, then for each
// mapping either syncs a single note or recursively syncs all notes
// under a prefix. Uses the same timestamp+hash comparison as Phase 1.
func NoteSync(client *api.Client) error {
    var mappingsResult syncMappingsResponse
    if err := client.Post("/agent/sync-mappings", map[string]interface{}{}, &mappingsResult); err != nil {
        // Non-fatal — don't block other sync phases
        fmt.Fprintf(os.Stderr, "Note sync error: %s\n", err)
        return nil
    }

    if len(mappingsResult.Mappings) == 0 {
        return nil
    }

    excludeSlugs := make(map[string]bool)
    for _, s := range mappingsResult.ExcludeSlugs {
        excludeSlugs[s] = true
    }

    totalPulled := 0
    totalPushed := 0
    totalUnchanged := 0
    totalDeleted := 0

    for _, mapping := range mappingsResult.Mappings {
        localDir := mapping.LocalPath
        slugPrefix := mapping.Slug
        namespace := mapping.Namespace

        if err := os.MkdirAll(localDir, 0755); err != nil {
            fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR creating dir %s: %s\n", localDir, err)
            continue
        }

        // Prefix mapping ends with '/' or is empty; otherwise it's a single note
        isPrefix := slugPrefix == "" || strings.HasSuffix(slugPrefix, "/")

        if !isPrefix {
            // Check if the slug's last segment is excluded
            lastSegment := lastSlugSegment(slugPrefix)
            if excludeSlugs[lastSegment] {
                continue
            }
            result := syncSingleNote(client, namespace, slugPrefix, localDir)
            totalPulled += result.pulled
            totalPushed += result.pushed
            totalUnchanged += result.unchanged
        } else {
            result := syncNotePrefix(client, namespace, slugPrefix, localDir, excludeSlugs)
            totalPulled += result.pulled
            totalPushed += result.pushed
            totalUnchanged += result.unchanged
            totalDeleted += result.deleted
        }
    }

    if totalPulled > 0 || totalPushed > 0 || totalDeleted > 0 {
        summary := fmt.Sprintf("Note sync: %d pulled, %d pushed, %d unchanged", totalPulled, totalPushed, totalUnchanged)
        if totalDeleted > 0 {
            summary += fmt.Sprintf(", %d deleted", totalDeleted)
        }
        fmt.Println(summary)
    }

    return nil
}

type syncResult struct {
    pulled    int
    pushed    int
    unchanged int
    deleted   int
}

// syncSingleNote syncs one remote note to one local file.
func syncSingleNote(client *api.Client, namespace string, slug string, localDir string) syncResult {
    var result syncResult

    // Derive filename from the slug's last segment
    baseName := lastSlugSegment(slug)
    if !strings.Contains(baseName, ".") {
        baseName += ".md"
    }
    filePath, pathErr := safePath(localDir, baseName)
    if pathErr != nil {
        fmt.Fprintf(os.Stderr, "  SKIP unsafe path from slug %s: %s\n", slug, pathErr)
        return result
    }

    // Try to read the remote note
    var remoteNote *noteDocument
    var readResp noteDocument
    err := client.Post("/documents/read", map[string]string{
        "namespace": namespace,
        "slug":      slug,
    }, &readResp)
    if err != nil {
        if !api.IsNotFound(err) {
            fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR reading %s: %s\n", slug, err)
            return result
        }
        // 404 = doesn't exist remotely
    } else {
        remoteNote = &readResp
    }

    _, localErr := os.Stat(filePath)
    localExists := localErr == nil

    if remoteNote != nil && !localExists {
        // Pull: remote exists, local doesn't
        if err := os.WriteFile(filePath, []byte(remoteNote.Content), 0644); err != nil {
            fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR writing %s: %s\n", filePath, err)
            return result
        }
        if remoteNote.UpdatedAt != "" {
            setMtime(filePath, remoteNote.UpdatedAt)
        }
        fmt.Printf("  PULL %s → %s\n", slug, filePath)
        result.pulled++
    } else if remoteNote == nil && localExists {
        // Push: local exists, remote doesn't
        content, err := os.ReadFile(filePath)
        if err != nil {
            return result
        }
        title := extractSyncTitle(string(content), baseName)
        var doc noteDocument
        err = client.Post("/documents/save", map[string]interface{}{
            "namespace": namespace,
            "slug":      slug,
            "title":     title,
            "content":   string(content),
        }, &doc)
        if err != nil {
            fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR saving %s: %s\n", slug, err)
            return result
        }
        if doc.UpdatedAt != "" {
            setMtime(filePath, doc.UpdatedAt)
        }
        fmt.Printf("  PUSH %s ← %s\n", slug, filePath)
        result.pushed++
    } else if remoteNote != nil && localExists {
        // Both exist — compare timestamps, then hashes
        info, err := os.Stat(filePath)
        if err != nil {
            return result
        }
        remoteTime := parseTimestamp(remoteNote.UpdatedAt)
        localTime := info.ModTime()

        // If remote timestamp is present but unparseable, skip — don't risk
        // a wrong push/pull decision based on a zero-value time.
        if remoteNote.UpdatedAt != "" && remoteTime.IsZero() {
            fmt.Fprintf(os.Stderr, "  SKIP %s: unparseable remote timestamp %q\n", slug, remoteNote.UpdatedAt)
            return result
        }

        if remoteTime.Equal(localTime) {
            result.unchanged++
        } else {
            localContent, err := os.ReadFile(filePath)
            if err != nil {
                return result
            }
            localHash := MD5Hash(string(localContent))
            remoteHash := MD5Hash(remoteNote.Content)

            if localHash == remoteHash {
                // Content identical — realign mtime
                setMtime(filePath, remoteNote.UpdatedAt)
                result.unchanged++
            } else if remoteTime.After(localTime) {
                if err := os.WriteFile(filePath, []byte(remoteNote.Content), 0644); err != nil {
                    return result
                }
                setMtime(filePath, remoteNote.UpdatedAt)
                fmt.Printf("  PULL %s → %s\n", slug, filePath)
                result.pulled++
            } else {
                title := extractSyncTitle(string(localContent), baseName)
                var doc noteDocument
                err = client.Post("/documents/save", map[string]interface{}{
                    "namespace": namespace,
                    "slug":      slug,
                    "title":     title,
                    "content":   string(localContent),
                }, &doc)
                if err != nil {
                    fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR saving %s: %s\n", slug, err)
                    return result
                }
                if doc.UpdatedAt != "" {
                    setMtime(filePath, doc.UpdatedAt)
                }
                fmt.Printf("  PUSH %s ← %s\n", slug, filePath)
                result.pushed++
            }
        }
    }

    return result
}

// syncNotePrefix syncs all notes under a slug prefix to files in a local directory.
func syncNotePrefix(client *api.Client, namespace string, prefix string, localDir string, excludeSlugs map[string]bool) syncResult {
    var result syncResult

    // List remote notes under the prefix, including soft-deleted
    var listResp struct {
        Notes []noteListEntry `json:"notes"`
    }
    err := client.Post("/documents/list", map[string]interface{}{
        "namespace":       namespace,
        "prefix":          prefix,
        "limit":           500,
        "include_deleted": true,
    }, &listResp)
    if err != nil {
        fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR listing %s: %s\n", prefix, err)
        return result
    }

    // Build remote map: relative path → note metadata
    remoteByRelPath := make(map[string]*noteListEntry)
    deletedRelPaths := make(map[string]bool)

    for i, note := range listResp.Notes {
        if !strings.HasPrefix(note.Slug, prefix) {
            continue
        }
        lastSeg := lastSlugSegment(note.Slug)
        if excludeSlugs[lastSeg] {
            continue
        }
        relPath := note.Slug[len(prefix):]
        if !strings.Contains(relPath, ".") {
            relPath += ".md"
        }
        if note.DeletedAt != "" {
            deletedRelPaths[relPath] = true
        } else {
            remoteByRelPath[relPath] = &listResp.Notes[i]
        }
    }

    // Scan local directory recursively
    localByRelPath := make(map[string]*localFile)
    scanDir(localDir, "", localByRelPath, excludeSlugs)

    // Merge all known relative paths
    allRelPaths := make(map[string]bool)
    for k := range remoteByRelPath {
        allRelPaths[k] = true
    }
    for k := range localByRelPath {
        allRelPaths[k] = true
    }

    for relPath := range allRelPaths {
        remote := remoteByRelPath[relPath]
        local := localByRelPath[relPath]

        // Derive slug from relative path
        slugSuffix := relPath
        if strings.HasSuffix(slugSuffix, ".md") {
            slugSuffix = slugSuffix[:len(slugSuffix)-3]
        }
        slug := prefix + slugSuffix
        localPath := ""
        if local != nil {
            localPath = local.fullPath
        } else {
            var pathErr error
            localPath, pathErr = safePath(localDir, relPath)
            if pathErr != nil {
                fmt.Fprintf(os.Stderr, "  SKIP unsafe path from slug %s: %s\n", slug, pathErr)
                continue
            }
        }

        if remote != nil && local == nil {
            // Pull: exists remotely but not locally
            var fullNote noteDocument
            err := client.Post("/documents/read", map[string]string{
                "namespace": namespace,
                "slug":      remote.Slug,
            }, &fullNote)
            if err != nil {
                fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR reading %s: %s\n", remote.Slug, err)
                continue
            }
            parentDir := filepath.Dir(localPath)
            if err := os.MkdirAll(parentDir, 0755); err != nil {
                continue
            }
            if err := os.WriteFile(localPath, []byte(fullNote.Content), 0644); err != nil {
                continue
            }
            if fullNote.UpdatedAt != "" {
                setMtime(localPath, fullNote.UpdatedAt)
            }
            fmt.Printf("  PULL %s → %s\n", slug, localPath)
            result.pulled++

        } else if remote == nil && local != nil {
            if deletedRelPaths[relPath] {
                // Deleted remotely — propagate
                _ = os.Remove(local.fullPath)
                fmt.Printf("  DELETE (remote deleted) %s\n", local.fullPath)
                result.deleted++
            } else {
                // Push: local exists, remote never existed
                title := extractSyncTitle(local.content, relPath)
                var doc noteDocument
                err := client.Post("/documents/save", map[string]interface{}{
                    "namespace": namespace,
                    "slug":      slug,
                    "title":     title,
                    "content":   local.content,
                }, &doc)
                if err != nil {
                    fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR saving %s: %s\n", slug, err)
                    continue
                }
                if doc.UpdatedAt != "" {
                    setMtime(local.fullPath, doc.UpdatedAt)
                }
                fmt.Printf("  PUSH %s ← %s\n", slug, local.fullPath)
                result.pushed++
            }

        } else if remote != nil && local != nil {
            // Both exist — compare timestamps, then hashes
            remoteTime := parseTimestamp(remote.UpdatedAt)
            localTime := local.mtime

            // If remote timestamp is present but unparseable, skip
            if remote.UpdatedAt != "" && remoteTime.IsZero() {
                fmt.Fprintf(os.Stderr, "  SKIP %s: unparseable remote timestamp %q\n", slug, remote.UpdatedAt)
                continue
            }

            if remoteTime.Equal(localTime) {
                result.unchanged++
            } else {
                localHash := MD5Hash(local.content)
                remoteHash := remote.ContentHash

                if remoteHash != "" && localHash == remoteHash {
                    // Content identical — realign mtime
                    setMtime(local.fullPath, remote.UpdatedAt)
                    result.unchanged++
                } else if remoteTime.After(localTime) {
                    var fullNote noteDocument
                    err := client.Post("/documents/read", map[string]string{
                        "namespace": namespace,
                        "slug":      remote.Slug,
                    }, &fullNote)
                    if err != nil {
                        fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR reading %s: %s\n", remote.Slug, err)
                        continue
                    }
                    if err := os.WriteFile(local.fullPath, []byte(fullNote.Content), 0644); err != nil {
                        continue
                    }
                    if fullNote.UpdatedAt != "" {
                        setMtime(local.fullPath, fullNote.UpdatedAt)
                    }
                    fmt.Printf("  PULL %s → %s\n", slug, local.fullPath)
                    result.pulled++
                } else {
                    title := extractSyncTitle(local.content, relPath)
                    var doc noteDocument
                    err := client.Post("/documents/save", map[string]interface{}{
                        "namespace": namespace,
                        "slug":      slug,
                        "title":     title,
                        "content":   local.content,
                    }, &doc)
                    if err != nil {
                        fmt.Fprintf(os.Stderr, "  NOTE SYNC ERROR saving %s: %s\n", slug, err)
                        continue
                    }
                    if doc.UpdatedAt != "" {
                        setMtime(local.fullPath, doc.UpdatedAt)
                    }
                    fmt.Printf("  PUSH %s ← %s\n", slug, local.fullPath)
                    result.pushed++
                }
            }
        }
    }

    return result
}

// --- Local directory scanning ---

type localFile struct {
    fullPath string
    content  string
    mtime    time.Time
}

func scanDir(dir string, relBase string, result map[string]*localFile, excludeSlugs map[string]bool) {
    entries, err := os.ReadDir(dir)
    if err != nil {
        return
    }
    for _, entry := range entries {
        if strings.HasPrefix(entry.Name(), ".") {
            continue
        }
        // Skip files whose base name (minus extension) matches an excluded slug
        baseName := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
        if excludeSlugs[baseName] {
            continue
        }

        fullPath := filepath.Join(dir, entry.Name())
        relPath := entry.Name()
        if relBase != "" {
            relPath = relBase + "/" + entry.Name()
        }

        if entry.IsDir() {
            scanDir(fullPath, relPath, result, excludeSlugs)
        } else {
            info, err := entry.Info()
            if err != nil {
                continue
            }
            content, err := os.ReadFile(fullPath)
            if err != nil {
                continue
            }
            result[relPath] = &localFile{
                fullPath: fullPath,
                content:  string(content),
                mtime:    info.ModTime(),
            }
        }
    }
}

// --- Types ---

type syncMappingsResponse struct {
    Mappings     []syncMapping `json:"mappings"`
    ExcludeSlugs []string      `json:"exclude_slugs"`
}

type syncMapping struct {
    LocalPath string `json:"local_path"`
    Slug      string `json:"slug"`
    Namespace string `json:"namespace"`
}

type noteDocument struct {
    Slug      string `json:"slug"`
    Content   string `json:"content"`
    UpdatedAt string `json:"updated_at"`
}

type noteListEntry struct {
    Slug        string `json:"slug"`
    UpdatedAt   string `json:"updated_at"`
    ContentHash string `json:"content_hash"`
    DeletedAt   string `json:"deleted_at"`
}

// --- Helpers ---

var frontmatterNameRegex = regexp.MustCompile(`(?s)^---\s*\n.*?name:\s*(.+)\n.*?---`)
var headingRegex = regexp.MustCompile(`(?m)^#\s+(.+)`)

func extractSyncTitle(content string, filename string) string {
    if content != "" {
        if m := frontmatterNameRegex.FindStringSubmatch(content); len(m) > 1 {
            return strings.TrimSpace(m[1])
        }
        if m := headingRegex.FindStringSubmatch(content); len(m) > 1 {
            return strings.TrimSpace(m[1])
        }
    }
    // Fall back to filename without extension and path
    base := filepath.Base(filename)
    base = strings.TrimSuffix(base, ".md")
    base = strings.ReplaceAll(base, "_", " ")
    base = strings.ReplaceAll(base, "-", " ")
    return base
}

func lastSlugSegment(slug string) string {
    idx := strings.LastIndex(slug, "/")
    if idx >= 0 {
        return slug[idx+1:]
    }
    return slug
}

// safePath validates that a derived local path stays within the given root
// directory. Returns the cleaned absolute path, or an error if the path
// would escape the root (e.g., via ".." traversal or absolute slug components).
func safePath(root string, relPath string) (string, error) {
    absRoot, err := filepath.Abs(root)
    if err != nil {
        return "", fmt.Errorf("resolve root: %w", err)
    }
    // Convert forward slashes from slugs to OS separators
    joined := filepath.Join(absRoot, filepath.FromSlash(relPath))
    cleaned := filepath.Clean(joined)

    // Ensure the cleaned path is under the root
    if !strings.HasPrefix(cleaned, absRoot+string(filepath.Separator)) && cleaned != absRoot {
        return "", fmt.Errorf("path %q escapes root %q", relPath, root)
    }
    return cleaned, nil
}
