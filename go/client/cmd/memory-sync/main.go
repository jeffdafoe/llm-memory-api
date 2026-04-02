// memory-sync — Bidirectional sync between local memory files and remote
// notes, plus conversation log upload, via the memory API.
//
// Reads .agent.json for auth credentials, scans a project directory for:
//   - {project-dir}/memory/*.md — note files (bidirectional sync)
//   - {project-dir}/*.jsonl — Claude Code session logs (one-way upload)
//
// Usage:
//
//	memory-sync --project-dir <path>
//	            [--config <path-to-.agent.json>]
//	            [--user <username>]
//	            [--notes-only]
package main

import (
    "flag"
    "fmt"
    "os"
    "os/signal"
    "path/filepath"
    "syscall"

    "github.com/jeffdafoe/llm-memory-api/go/client/internal/api"
    "github.com/jeffdafoe/llm-memory-api/go/client/internal/config"
    "github.com/jeffdafoe/llm-memory-api/go/client/internal/memsync"
    "github.com/jeffdafoe/llm-memory-api/go/client/internal/selfupdate"
)

func main() {
    // --- Flags ---
    projectDir := flag.String("project-dir", "", "Project directory containing memory/ and session .jsonl files")
    configPath := flag.String("config", "", "Path to .agent.json (default: .agent.json in current directory)")
    userName := flag.String("user", "user", "Label for user messages in conversation logs")
    notesOnly := flag.Bool("notes-only", false, "Skip memory sync and conversation upload; only run note directory sync")
    showVersion := flag.Bool("version", false, "Print version and exit")
    noUpdate := flag.Bool("no-update", false, "Skip automatic update check")

    // Deprecated alias — accept silently
    flag.String("local-dir", "", "")
    flag.String("prefix", "", "")

    flag.Parse()

    if *showVersion {
        fmt.Printf("memory-sync v%s\n", selfupdate.Version)
        os.Exit(0)
    }

    // Check for updates before doing anything else
    if !*noUpdate {
        selfupdate.Check()
    }

    // Support deprecated --local-dir as alias for --project-dir
    if *projectDir == "" {
        localDir := flag.Lookup("local-dir")
        if localDir != nil && localDir.Value.String() != "" {
            *projectDir = localDir.Value.String()
        }
    }

    if *projectDir == "" {
        fmt.Fprintln(os.Stderr, "Usage: memory-sync --project-dir <path> [--config <path>] [--user <name>] [--notes-only]")
        os.Exit(1)
    }

    // Resolve config path
    cfgPath := *configPath
    if cfgPath == "" {
        cwd, err := os.Getwd()
        if err != nil {
            fmt.Fprintf(os.Stderr, "Failed to get working directory: %s\n", err)
            os.Exit(1)
        }
        cfgPath = filepath.Join(cwd, ".agent.json")
    }

    // Load config
    cfg, err := config.Load(cfgPath)
    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        os.Exit(1)
    }

    // Create API client (logs in)
    client, err := api.New(cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        os.Exit(1)
    }
    defer client.Logout()

    // Trap SIGINT/SIGTERM so logout runs on Ctrl+C
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-sigCh
        client.Logout()
        os.Exit(1)
    }()

    if !*notesOnly {
        // Phase 1: Memory sync
        retentionDays, err := memsync.MemorySyncWithConvConfig(client, *projectDir)
        if err != nil {
            fmt.Fprintf(os.Stderr, "Memory sync error: %s\n", err)
            os.Exit(1)
        }

        // Phase 2: Conversation sync (only if server returned retention config)
        if retentionDays > 0 {
            if err := memsync.ConversationSync(client, *projectDir, cfg.Agent, *userName, retentionDays); err != nil {
                fmt.Fprintf(os.Stderr, "Conversation sync error: %s\n", err)
                // Non-fatal — continue to Phase 3
            }
        }
    }

    // Phase 3: Note directory sync
    if err := memsync.NoteSync(client); err != nil {
        fmt.Fprintf(os.Stderr, "Note sync error: %s\n", err)
    }
}
