// mail-send — Send mail to another agent via the memory API, reading the
// body from a local file so large bodies (diffs, review reports, long
// analyses) never have to be passed as a string argument.
//
// Reads .agent.json for auth credentials and POSTs /mail/send directly.
// Matches the distribution pattern of memory-sync and discuss — drop the
// binary in C:/dev/bin/ (or /usr/local/bin on Linux) and call it from any
// harness.
//
// Usage:
//
//	mail-send --to <agent> --subject <subject> --body-file <path>
//	          [--config <path-to-.agent.json>]
//	          [--in-reply-to <mail-uuid>]
//	          [--from <agent>]
package main

import (
    "flag"
    "fmt"
    "os"
    "path/filepath"

    "github.com/jeffdafoe/llm-memory-api/go/client/internal/api"
    "github.com/jeffdafoe/llm-memory-api/go/client/internal/config"
    "github.com/jeffdafoe/llm-memory-api/go/client/internal/selfupdate"
)

// sendResponse mirrors the /mail/send response shape. We only surface the
// id so callers can thread replies with --in-reply-to later.
type sendResponse struct {
    ID        string `json:"id"`
    ToAgent   string `json:"to_agent"`
    FromAgent string `json:"from_agent"`
    Subject   string `json:"subject"`
    SentAt    string `json:"sent_at"`
}

func main() {
    to := flag.String("to", "", "Recipient agent name (e.g. code_review, work)")
    subject := flag.String("subject", "", "Subject line")
    bodyFile := flag.String("body-file", "", "Path to a file whose contents are the mail body")
    bodyInline := flag.String("body", "", "Mail body as an inline string (alternative to --body-file)")
    inReplyTo := flag.String("in-reply-to", "", "UUID of the mail being replied to (optional, for threading)")
    from := flag.String("from", "", "Override sender agent name (defaults to the agent in .agent.json)")
    configPath := flag.String("config", "", "Path to .agent.json (default: .agent.json in current directory)")
    showVersion := flag.Bool("version", false, "Print version and exit")
    noUpdate := flag.Bool("no-update", false, "Skip automatic update check")
    flag.Parse()

    if *showVersion {
        fmt.Printf("mail-send v%s\n", selfupdate.Version)
        os.Exit(0)
    }

    if !*noUpdate {
        selfupdate.Check("mail-send")
    }

    if *to == "" || *subject == "" {
        fmt.Fprintln(os.Stderr, "Usage: mail-send --to <agent> --subject <subject> --body-file <path>")
        fmt.Fprintln(os.Stderr, "                 [--body <inline-string>] [--in-reply-to <uuid>]")
        fmt.Fprintln(os.Stderr, "                 [--from <agent>] [--config <path>]")
        os.Exit(1)
    }

    if (*bodyFile == "" && *bodyInline == "") || (*bodyFile != "" && *bodyInline != "") {
        fmt.Fprintln(os.Stderr, "Exactly one of --body-file or --body must be provided.")
        os.Exit(1)
    }

    // Load body — either from file or inline flag.
    var body string
    if *bodyFile != "" {
        data, err := os.ReadFile(*bodyFile)
        if err != nil {
            fmt.Fprintf(os.Stderr, "Read body file: %s\n", err)
            os.Exit(1)
        }
        body = string(data)
    } else {
        body = *bodyInline
    }

    // Resolve config path.
    cfgPath := *configPath
    if cfgPath == "" {
        cwd, err := os.Getwd()
        if err != nil {
            fmt.Fprintf(os.Stderr, "Get working directory: %s\n", err)
            os.Exit(1)
        }
        cfgPath = filepath.Join(cwd, ".agent.json")
    }

    cfg, err := config.Load(cfgPath)
    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        os.Exit(1)
    }

    client, err := api.New(cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        os.Exit(1)
    }
    defer client.Logout()

    // Build request body. The API enforces identity when from_agent is
    // set — it must match the authenticated agent. We default to the
    // config agent if --from is not supplied.
    fromAgent := *from
    if fromAgent == "" {
        fromAgent = cfg.Agent
    }

    req := map[string]interface{}{
        "to_agent":   *to,
        "from_agent": fromAgent,
        "subject":    *subject,
        "body":       body,
    }
    if *inReplyTo != "" {
        req["in_reply_to"] = *inReplyTo
    }

    var resp sendResponse
    if err := client.Post("/mail/send", req, &resp); err != nil {
        fmt.Fprintf(os.Stderr, "Send failed: %s\n", err)
        os.Exit(1)
    }

    // Terse success output — just the ID so scripts can capture it easily.
    fmt.Printf("Sent %s -> %s (id: %s)\n", resp.FromAgent, resp.ToAgent, resp.ID)
}
