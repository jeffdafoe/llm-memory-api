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
//	          [--attach <image> ...]
//
// --attach adds an image (PNG, JPEG, WebP or GIF; up to 8, 3 MiB together)
// and may be given more than once. A virtual agent that receives the mail
// sees the images with the text (LLM-1040).
package main

import (
    "encoding/base64"
    "flag"
    "fmt"
    "os"
    "path/filepath"
    "strings"

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

// attachFlag collects every --attach path, in order. flag.Var takes any type
// with String and Set, and calls Set once per occurrence of the flag.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

func (a *attachFlag) Set(path string) error {
    *a = append(*a, path)
    return nil
}

// imageTypes maps a file extension to the media type the API expects. The
// server checks the bytes too, so a mislabelled file is refused there.
var imageTypes = map[string]string{
    ".png":  "image/png",
    ".jpg":  "image/jpeg",
    ".jpeg": "image/jpeg",
    ".webp": "image/webp",
    ".gif":  "image/gif",
}

// loadAttachments reads each image into the /mail/send attachments shape.
func loadAttachments(paths []string) ([]map[string]string, error) {
    out := make([]map[string]string, 0, len(paths))
    for _, p := range paths {
        mediaType, ok := imageTypes[strings.ToLower(filepath.Ext(p))]
        if !ok {
            return nil, fmt.Errorf("%s: not a .png, .jpg, .jpeg, .webp or .gif file", p)
        }
        data, err := os.ReadFile(p)
        if err != nil {
            return nil, err
        }
        out = append(out, map[string]string{
            "filename":    filepath.Base(p),
            "media_type":  mediaType,
            "data_base64": base64.StdEncoding.EncodeToString(data),
        })
    }
    return out, nil
}

func main() {
    var attach attachFlag
    flag.Var(&attach, "attach", "Image to attach (PNG, JPEG, WebP or GIF); repeat for more")
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
        fmt.Fprintln(os.Stderr, "                 [--from <agent>] [--config <path>] [--attach <image> ...]")
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

    attachments, err := loadAttachments(attach)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Attach: %s\n", err)
        os.Exit(1)
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
    if len(attachments) > 0 {
        req["attachments"] = attachments
    }

    var resp sendResponse
    if err := client.Post("/mail/send", req, &resp); err != nil {
        fmt.Fprintf(os.Stderr, "Send failed: %s\n", err)
        os.Exit(1)
    }

    // Terse success output — just the ID so scripts can capture it easily.
    if len(attachments) > 0 {
        fmt.Printf("Sent %s -> %s (id: %s, %d image(s))\n", resp.FromAgent, resp.ToAgent, resp.ID, len(attachments))
        return
    }
    fmt.Printf("Sent %s -> %s (id: %s)\n", resp.FromAgent, resp.ToAgent, resp.ID)
}
