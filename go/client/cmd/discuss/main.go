// discuss — Discussion transport binary for the memory API.
//
// Combines the functionality of discuss.js (transport) and
// discuss-launch.js (launcher) into a single binary. In normal mode
// (the default), it spawns itself as a background transport process,
// polls for the prompt file, and outputs its contents to stdout.
// In --transport mode, it runs the transport directly (used when
// the binary re-spawns itself).
//
// Usage:
//
//	discuss create --config .agent.json --topic "..." --other <agent> [--optional <agent>]
//	discuss join --config .agent.json [discussion-id]
//	discuss create --transport --config .agent.json --topic "..." --other <agent>
//
// The --config flag points to .agent.json which provides agent name,
// passphrase, api_url, and work_dir.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/client/internal/discuss"
)

// Embed the discussion prompt template into the binary so it doesn't
// need to exist on disk at runtime. The source file lives at
// templates/discussion-prompt.tpl in the repo root — this copy in
// cmd/discuss/ must be kept in sync.
//
//go:embed discussion-prompt.tpl
var embeddedTemplate string

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	if command != "create" && command != "join" {
		printUsage()
		os.Exit(1)
	}

	// Parse flags from os.Args[2:]
	args := os.Args[2:]
	opts := parseArgs(args, command)

	// Load config from .agent.json
	cfg := discuss.DefaultConfig()
	cfg.Initiator = (command == "create")

	// Apply CLI overrides before loading config file
	if opts.agent != "" {
		cfg.Agent = opts.agent
	}
	if opts.passphrase != "" {
		cfg.Passphrase = opts.passphrase
	}
	if opts.apiURL != "" {
		cfg.APIURL = opts.apiURL
	}
	if opts.workDir != "" {
		cfg.WorkDirBase = opts.workDir
	}

	if opts.configPath != "" {
		if err := cfg.LoadAgentConfig(opts.configPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			os.Exit(1)
		}
	}

	// Apply remaining options
	cfg.Topic = opts.topic
	cfg.Others = opts.others
	cfg.OptionalParticipants = opts.optional
	cfg.Context = opts.context
	cfg.ContextFile = opts.contextFile
	if opts.mode != "" {
		cfg.Mode = opts.mode
	}
	if opts.maxMessages > 0 {
		cfg.MaxMessages = opts.maxMessages
	}
	if opts.timeoutMinutes > 0 {
		cfg.TimeoutMinutes = opts.timeoutMinutes
	}
	if opts.joinTimeout > 0 {
		cfg.JoinTimeout = opts.joinTimeout
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}

	if opts.transportMode {
		// Direct transport mode — run the full discussion lifecycle
		runTransport(cfg, command, opts.discussionID)
	} else {
		// Launcher mode — spawn transport as background process, poll for prompt
		runLauncher(cfg, command, opts)
	}
}

// runTransport is the main entry point when running in --transport mode.
// It handles the full discussion lifecycle: login, create/join, wait,
// proxy, transport loop, cleanup.
func runTransport(cfg *discuss.Config, command string, discussionID int) {
	logger := discuss.NewLogger("") // will be set after workDir is known

	// Health check
	healthURL := strings.TrimSuffix(cfg.APIURL, "/v1") + "/health"
	healthURL = strings.TrimSuffix(healthURL, "/") + "/health"
	// Normalize: if APIURL ends with /v1, strip it and add /health
	if strings.HasSuffix(cfg.APIURL, "/v1") {
		healthURL = cfg.APIURL[:len(cfg.APIURL)-3] + "/health"
	} else if strings.HasSuffix(cfg.APIURL, "/v1/") {
		healthURL = cfg.APIURL[:len(cfg.APIURL)-4] + "/health"
	} else {
		healthURL = cfg.APIURL + "/health"
	}

	client := discuss.NewAPIClient(cfg.APIURL, cfg.Agent, cfg.Passphrase, logger)
	if err := client.Get(healthURL); err != nil {
		fmt.Fprintf(os.Stderr, "API at %s is unreachable: %s\n", cfg.APIURL, err)
		os.Exit(1)
	}

	if err := client.Login(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}

	// Fetch server config for discussion defaults
	discuss.ApplyServerConfig(client, cfg, logger)

	// Setup based on command
	if command == "create" {
		if len(cfg.Others) == 0 {
			fmt.Fprintf(os.Stderr, "create requires at least one --other participant\n")
			os.Exit(1)
		}
		if cfg.Topic == "" {
			fmt.Fprintf(os.Stderr, "create requires --topic\n")
			os.Exit(1)
		}
		var err error
		discussionID, err = discuss.SetupCreate(client, cfg, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Create failed: %s\n", err)
			client.Logout()
			os.Exit(1)
		}
	} else {
		// Join mode
		if discussionID == 0 {
			var err error
			discussionID, err = discuss.AutoDiscoverDiscussion(client, cfg, logger)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s\n", err)
				client.Logout()
				os.Exit(1)
			}
		}
		var err error
		discussionID, err = discuss.SetupJoin(client, cfg, logger, discussionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Join failed: %s\n", err)
			client.Logout()
			os.Exit(1)
		}
	}

	// Set workDir now that we have the discussion ID
	cfg.SetWorkDir(discussionID)
	paths := discuss.InitPaths(cfg.WorkDir)
	logger.SetLogFile(paths.LogFile)

	if err := os.MkdirAll(cfg.WorkDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Create work dir: %s\n", err)
		client.Logout()
		os.Exit(1)
	}
	if err := paths.EnsureDirectories(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		client.Logout()
		os.Exit(1)
	}

	paths.CleanWorkDir()
	paths.KillStalePID()
	if err := paths.AcquireLock(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		client.Logout()
		os.Exit(1)
	}
	paths.WritePollScript(cfg.WorkDir)

	// Wait for all participants
	if err := discuss.WaitForReady(client, discussionID, cfg, logger); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		paths.ReleaseLock()
		client.Logout()
		os.Exit(1)
	}

	// Signal activity
	client.Post("/agent/activity/start", map[string]interface{}{"agent": cfg.Agent}, nil)

	paths.InitTranscript(cfg.Agent, cfg.OtherAgents())

	// Start proxy server
	startTime := time.Now()
	others := cfg.OtherAgents()
	convergence := discuss.NewConvergence(
		append([]string{cfg.Agent}, others...),
		cfg.MaxRounds, logger, paths,
	)
	proxy, err := discuss.NewProxyServer(client, discussionID, cfg.Agent,
		convergence, paths, logger, startTime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Start proxy: %s\n", err)
		paths.ReleaseLock()
		client.Logout()
		os.Exit(1)
	}
	proxy.Start()

	// Generate prompt
	discuss.GeneratePrompt(cfg, paths, discussionID, proxy.Port(), logger, embeddedTemplate)

	// Log summary
	logger.Log("Discussion #%d ready", discussionID)
	logger.Log("Mode: %s, Agent: %s, Others: %s",
		command, cfg.Agent, strings.Join(others, ", "))
	logger.Log("Proxy: 127.0.0.1:%d", proxy.Port())
	logger.Log("Prompt: %s", paths.PromptFile)

	// Handle clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Log("Shutting down...")
		paths.ReleaseLock()
		client.Logout()
		proxy.Close()
		os.Exit(0)
	}()

	// Create and run transport
	transport := discuss.NewTransport(client, cfg, paths, logger,
		convergence, proxy, discussionID)
	transport.RestoreState()
	transport.Run()

	// Save transcript and result
	transport.SaveTranscript()
	transport.SaveResult()

	// Clean exit
	paths.ReleaseLock()
	client.Logout()
	proxy.Close()

	if transport.IsTerminated() {
		os.Exit(2)
	}
	os.Exit(0)
}

// runLauncher spawns the transport as a background process, polls for
// the prompt file, and outputs its contents to stdout.
func runLauncher(cfg *discuss.Config, command string, opts *options) {
	workDir := cfg.WorkDirBase
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "llm")
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Create work dir: %s\n", err)
		os.Exit(1)
	}

	// Clean up stale context temp files
	entries, _ := os.ReadDir(workDir)
	for _, e := range entries {
		matched, _ := regexp.MatchString(`^discuss-context-\d+\.md$`, e.Name())
		if matched {
			os.Remove(filepath.Join(workDir, e.Name()))
		}
	}

	// If --context-file is provided, copy to a unique temp file
	childArgs := buildChildArgs(command, opts)
	if opts.contextFile != "" {
		uniquePath := filepath.Join(workDir, fmt.Sprintf("discuss-context-%d.md", time.Now().UnixMilli()))
		data, err := os.ReadFile(opts.contextFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: Could not copy context file: %s\n", err)
		} else {
			os.WriteFile(uniquePath, data, 0644)
			// Replace context-file in child args
			for i, a := range childArgs {
				if a == "--context-file" && i+1 < len(childArgs) {
					childArgs[i+1] = uniquePath
					break
				}
			}
			fmt.Fprintf(os.Stderr, "Context copied to %s\n", uniquePath)
		}
	}

	logFile := filepath.Join(workDir, "discuss-transport.log")
	os.Remove(logFile) // clear old log

	// Spawn self as background transport process
	exe, _ := os.Executable()
	transportArgs := append(childArgs, "--transport")

	logFd, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open log file: %s\n", err)
		os.Exit(1)
	}

	cmd := exec.Command(exe, transportArgs...)
	cmd.Stdout = logFd
	cmd.Stderr = logFd
	// On Windows, we can't use SysProcAttr for detaching the same way,
	// but the process will outlive us since we exit after reading the prompt.
	if err := cmd.Start(); err != nil {
		logFd.Close()
		fmt.Fprintf(os.Stderr, "Start transport: %s\n", err)
		os.Exit(1)
	}
	logFd.Close()

	fmt.Fprintf(os.Stderr, "Transport PID: %d\n", cmd.Process.Pid)

	// Release the child so it's not killed when we exit
	cmd.Process.Release()

	// Poll for "Prompt written to <path>" in the transport log
	timeoutSec := 600
	promptRe := regexp.MustCompile(`Prompt written to (.+)`)

	for elapsed := 0; elapsed < timeoutSec; elapsed += 2 {
		time.Sleep(2 * time.Second)

		data, err := os.ReadFile(logFile)
		if err != nil {
			continue
		}

		match := promptRe.FindSubmatch(data)
		if match != nil {
			promptPath := strings.TrimSpace(string(match[1]))
			prompt, err := os.ReadFile(promptPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to read prompt at %s: %s\n", promptPath, err)
				os.Exit(1)
			}
			fmt.Print(string(prompt))
			os.Exit(0)
		}
	}

	fmt.Fprintf(os.Stderr, "ERROR: prompt.txt not found after %ds\n", timeoutSec)
	data, _ := os.ReadFile(logFile)
	if len(data) > 0 {
		fmt.Fprintf(os.Stderr, "--- Transport log ---\n%s\n", string(data))
	}
	os.Exit(1)
}

// --- Argument parsing ---

type options struct {
	configPath     string
	topic          string
	others         []string
	optional       []string
	context        string
	contextFile    string
	mode           string
	workDir        string
	maxMessages    int
	timeoutMinutes int
	joinTimeout    int
	agent          string
	passphrase     string
	apiURL         string
	discussionID  int
	transportMode bool
}

func parseArgs(args []string, command string) *options {
	opts := &options{}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 < len(args) {
				opts.configPath = args[i+1]
				i++
			}
		case "--topic":
			if i+1 < len(args) {
				opts.topic = args[i+1]
				i++
			}
		case "--other":
			if i+1 < len(args) {
				opts.others = append(opts.others, args[i+1])
				i++
			}
		case "--optional":
			if i+1 < len(args) {
				opts.optional = append(opts.optional, args[i+1])
				i++
			}
		case "--context":
			if i+1 < len(args) {
				opts.context = args[i+1]
				i++
			}
		case "--context-file":
			if i+1 < len(args) {
				opts.contextFile = args[i+1]
				i++
			}
		case "--mode":
			if i+1 < len(args) {
				opts.mode = args[i+1]
				i++
			}
		case "--work-dir":
			if i+1 < len(args) {
				opts.workDir = args[i+1]
				i++
			}
		case "--max-messages":
			if i+1 < len(args) {
				opts.maxMessages, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--timeout":
			if i+1 < len(args) {
				opts.timeoutMinutes, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--join-timeout":
			if i+1 < len(args) {
				opts.joinTimeout, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--agent":
			if i+1 < len(args) {
				opts.agent = args[i+1]
				i++
			}
		case "--passphrase":
			if i+1 < len(args) {
				opts.passphrase = args[i+1]
				i++
			}
		case "--api-url":
			if i+1 < len(args) {
				opts.apiURL = args[i+1]
				i++
			}
		case "--transport":
			opts.transportMode = true
		default:
			// Bare numeric argument for join command = discussion ID
			if command == "join" && opts.discussionID == 0 {
				if id, err := strconv.Atoi(args[i]); err == nil {
					opts.discussionID = id
				}
			}
		}
	}

	return opts
}

// buildChildArgs reconstructs the CLI arguments for the child transport
// process, preserving all flags from the original invocation.
func buildChildArgs(command string, opts *options) []string {
	args := []string{command}
	if opts.configPath != "" {
		args = append(args, "--config", opts.configPath)
	}
	if opts.topic != "" {
		args = append(args, "--topic", opts.topic)
	}
	for _, o := range opts.others {
		args = append(args, "--other", o)
	}
	for _, o := range opts.optional {
		args = append(args, "--optional", o)
	}
	if opts.context != "" {
		args = append(args, "--context", opts.context)
	}
	if opts.contextFile != "" {
		args = append(args, "--context-file", opts.contextFile)
	}
	if opts.mode != "" {
		args = append(args, "--mode", opts.mode)
	}
	if opts.workDir != "" {
		args = append(args, "--work-dir", opts.workDir)
	}
	if opts.maxMessages > 0 {
		args = append(args, "--max-messages", strconv.Itoa(opts.maxMessages))
	}
	if opts.timeoutMinutes > 0 {
		args = append(args, "--timeout", strconv.Itoa(opts.timeoutMinutes))
	}
	if opts.joinTimeout > 0 {
		args = append(args, "--join-timeout", strconv.Itoa(opts.joinTimeout))
	}
	if opts.agent != "" {
		args = append(args, "--agent", opts.agent)
	}
	if opts.passphrase != "" {
		args = append(args, "--passphrase", opts.passphrase)
	}
	if opts.apiURL != "" {
		args = append(args, "--api-url", opts.apiURL)
	}
	if opts.discussionID > 0 {
		args = append(args, strconv.Itoa(opts.discussionID))
	}
	return args
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: discuss create --config .agent.json --topic "..." --other <agent>
       discuss join --config .agent.json [discussion-id]

Spawns a discussion transport as a background process, waits for the
prompt file, and outputs its contents to stdout.

Options:
  --config <path>       Path to .agent.json
  --topic <text>        Discussion topic (required for create)
  --other <agent>       Required participant (repeatable)
  --optional <agent>    Optional participant (repeatable)
  --context <text>      Discussion context
  --context-file <path> Read context from file
  --mode <mode>         Discussion mode: realtime or async (default: realtime)
  --work-dir <path>     Working directory base
  --max-messages <n>    Max messages before timeout (default: 200)
  --timeout <minutes>   Max duration in minutes (default: 120)
  --join-timeout <sec>  Seconds to wait for invitation (default: 300)
  --agent <name>        Override agent name from config
  --passphrase <text>   Override passphrase from config
  --api-url <url>       Override API URL from config
`)
}
