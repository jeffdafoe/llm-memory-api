package discuss

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	subagentDeadThreshold = 3 * time.Minute
	stallWarningDuration  = 90 * time.Second
	statusCheckInterval   = 10 // check server-side status every N poll cycles
)

// Transport manages the main message-relay loop between the Claude
// subagent (via inbox/outbox files) and the remote discussion API
// (via chat messages).
type Transport struct {
	client       *APIClient
	cfg          *Config
	paths        *Paths
	logger       *Logger
	convergence  *Convergence
	proxy        *ProxyServer
	discussionID int

	// State
	seenIDs         map[int]bool
	turnCount       int
	lastDeliveredID int
	readySignaled   bool
	startTime       time.Time

	// Subagent death detection
	lastOutboxTime        time.Time
	lastInboxDeliveryTime time.Time
	subagentDeathReported bool
	stallWarningLogged    bool

	// Server-side status polling
	statusCheckCounter int
}

// NewTransport creates a transport for the given discussion.
func NewTransport(client *APIClient, cfg *Config, paths *Paths, logger *Logger,
	convergence *Convergence, proxy *ProxyServer, discussionID int) *Transport {

	return &Transport{
		client:       client,
		cfg:          cfg,
		paths:        paths,
		logger:       logger,
		convergence:  convergence,
		proxy:        proxy,
		discussionID: discussionID,
		seenIDs:      make(map[int]bool),
		startTime:    time.Now(),
		lastOutboxTime: time.Now(),
	}
}

// RestoreState loads persisted state from a previous run (crash recovery).
func (t *Transport) RestoreState() {
	prev := t.paths.ReadState()
	if prev == nil {
		return
	}
	for _, id := range prev.SeenIDs {
		t.seenIDs[id] = true
	}
	if prev.LastDeliveredID > 0 {
		t.lastDeliveredID = prev.LastDeliveredID
	}
}

// Run executes the main transport loop: poll for messages, relay to/from
// the subagent, check convergence, and handle timeouts.
func (t *Transport) Run() error {
	t.logger.Log("Discussion transport starting: %s <-> %s",
		t.cfg.Agent, strings.Join(t.cfg.OtherAgents(), ", "))
	t.logger.Log("Work dir: %s", t.cfg.WorkDir)
	t.logger.Log("Discussion ID: %d", t.discussionID)
	t.setStatus("POLLING")

	for {
		t.paths.TouchLock()

		// 1. Check done
		if t.paths.IsDone() {
			t.logger.Log("Done file detected. Discussion concluded.")
			t.setStatus("DONE")
			return nil
		}

		// 2. Check idle timeout
		if t.paths.IsIdleTimeout() {
			t.logger.Log("Idle timeout detected. Shutting down.")
			t.setStatus("IDLE_TIMEOUT")
			return nil
		}

		// 3. Check timeout
		if t.checkTimeout() {
			t.logger.Log("Timeout reached (%d turns, %dm). Writing timeout notice.",
				t.turnCount, t.cfg.TimeoutMinutes)
			t.paths.WriteInboxFile("timeout.txt", "[SYSTEM] Discussion timeout reached. Please wrap up.")
			t.setStatus("TIMEOUT")
			time.Sleep(30 * time.Second)
			if msg, ok := t.paths.CheckOutbox(); ok {
				t.sendMessage(msg)
			}
			t.setStatus("DONE")
			t.paths.WriteDone("timeout")
			return nil
		}

		// 4. Check outbox
		if msg, ok := t.paths.CheckOutbox(); ok {
			sizeKB := MessageSize(msg)
			if !t.lastInboxDeliveryTime.IsZero() {
				responseTime := int(time.Since(t.lastInboxDeliveryTime).Seconds())
				t.logger.Log("Outbox: %sKB, response time: %ds", sizeKB, responseTime)
			} else {
				t.logger.Log("Outbox: %sKB", sizeKB)
			}
			t.lastOutboxTime = time.Now()
			t.stallWarningLogged = false
			t.subagentDeathReported = false
			t.sendMessage(msg)
			time.Sleep(t.cfg.SendDelay)
		}

		// 5. Poll for incoming messages
		messages := t.receiveMessages()

		// 6-7. Process: dedup, write inbox, transcript
		allIDs, newIDs := t.processReceivedMessages(messages)

		// 8. Poll pending votes
		t.checkPendingVotes()

		// 9. Convergence detection
		result := t.convergence.Check(t.client, t.discussionID, t.cfg.Agent)
		if result == "terminate" {
			t.logger.Log("Convergence termination — exiting transport loop")
			t.waitForResult()
			t.paths.WriteDone("terminated")
			t.setStatus("TERMINATED")
			return nil
		}

		// 10. Periodic server-side status check
		t.statusCheckCounter++
		if t.statusCheckCounter >= statusCheckInterval {
			t.statusCheckCounter = 0
			if t.checkServerStatus() {
				return nil
			}
		}

		if len(allIDs) > 0 {
			// Delayed ack — wait for subagent to consume inbox files
			if len(newIDs) > 0 {
				t.waitForSubagentReads(newIDs)
			}

			// Ack all
			t.ackMessages(allIDs)

			// Record last-delivered-id
			maxID := 0
			for _, id := range allIDs {
				if id > maxID {
					maxID = id
				}
			}
			t.lastDeliveredID = maxID
			t.lastInboxDeliveryTime = time.Now()

			if !t.readySignaled {
				t.readySignaled = true
				t.logger.Log("Received first message — ready signaled")
			}

			t.setStatus("MESSAGE_RECEIVED")
		} else {
			time.Sleep(t.cfg.PollInterval)
		}

		// Stall warning
		if !t.stallWarningLogged &&
			!t.lastInboxDeliveryTime.IsZero() &&
			t.lastInboxDeliveryTime.After(t.lastOutboxTime) &&
			time.Since(t.lastInboxDeliveryTime) > stallWarningDuration {
			t.stallWarningLogged = true
			t.logger.Log("WARNING: No outbox response %ds after inbox delivery",
				int(time.Since(t.lastInboxDeliveryTime).Seconds()))
		}

		// Subagent death detection
		if !t.subagentDeathReported &&
			!t.lastInboxDeliveryTime.IsZero() &&
			t.lastInboxDeliveryTime.After(t.lastOutboxTime) &&
			time.Since(t.lastInboxDeliveryTime) > subagentDeadThreshold {
			t.subagentDeathReported = true
			t.logger.Log("Subagent appears dead — no outbox activity after inbox delivery")
			t.reportSubagentDead()
		}
	}
}

// TurnCount returns the number of messages sent.
func (t *Transport) TurnCount() int {
	return t.turnCount
}

// IsTerminated returns true if convergence detection terminated the discussion.
func (t *Transport) IsTerminated() bool {
	return t.convergence.State == StateTerminated
}

// waitForResult gives the subagent time to write result.md after
// receiving a shutdown notice. Polls for result.md with a 60-second
// timeout. Also drains any outbox messages while waiting.
func (t *Transport) waitForResult() {
	timeout := time.Duration(t.cfg.ResultTimeout) * time.Second
	poll := 2 * time.Second
	start := time.Now()

	for time.Since(start) < timeout {
		// Drain outbox while waiting
		if msg, ok := t.paths.CheckOutbox(); ok {
			t.sendMessage(msg)
		}

		// Check if result.md exists
		if _, err := os.Stat(t.paths.ResultFile); err == nil {
			t.logger.Log("result.md found after %ds", int(time.Since(start).Seconds()))
			return
		}

		time.Sleep(poll)
	}

	t.logger.Log("WARNING: result.md not found after %ds — proceeding with shutdown", int(timeout.Seconds()))
	// Final outbox drain
	if msg, ok := t.paths.CheckOutbox(); ok {
		t.sendMessage(msg)
	}
}

// --- Messaging ---

func (t *Transport) receiveMessages() []ChatMessage {
	body := map[string]interface{}{
		"agent":         t.cfg.Agent,
		"discussion_id": t.discussionID,
	}
	if t.lastDeliveredID > 0 {
		body["after_id"] = t.lastDeliveredID
	}

	var resp ChatReceiveResponse
	if err := t.client.Post("/chat/receive", body, &resp); err != nil {
		t.logger.Log("ERROR receiving: %s", err)
		return nil
	}
	return resp.Messages
}

func (t *Transport) processReceivedMessages(messages []ChatMessage) (allIDs, newIDs []int) {
	for _, msg := range messages {
		allIDs = append(allIDs, msg.ID)
		if t.seenIDs[msg.ID] {
			continue
		}
		t.seenIDs[msg.ID] = true
		newIDs = append(newIDs, msg.ID)

		content := fmt.Sprintf("From: %s\nSent: %s\n\n%s", msg.FromAgent, msg.SentAt, msg.Message)
		t.paths.WriteInboxFile(fmt.Sprintf("%d.txt", msg.ID), content)
		t.paths.AppendTranscript(msg.FromAgent, msg.Message)
		t.convergence.TrackMessage(msg.FromAgent)
		t.logger.Log("RECEIVED: id=%d from=%s", msg.ID, msg.FromAgent)
	}
	return
}

func (t *Transport) sendMessage(message string) {
	err := t.client.Post("/chat/send", map[string]interface{}{
		"from_agent":    t.cfg.Agent,
		"discussion_id": t.discussionID,
		"message":       message,
	}, nil)
	if err != nil {
		// Discussion already concluded — other side left, no recipients
		if strings.Contains(err.Error(), "NO_RECIPIENTS") {
			t.logger.Log("Send skipped — discussion already concluded (no recipients)")
			return
		}
		t.logger.Log("ERROR sending: %s", err)
		return
	}

	truncated := message
	if len(truncated) > 100 {
		truncated = truncated[:100]
	}
	t.logger.Log("SENT: %s...", truncated)
	t.paths.AppendTranscript(t.cfg.Agent, message)
	t.convergence.TrackMessage(t.cfg.Agent)
	t.turnCount++
}

func (t *Transport) ackMessages(ids []int) {
	if len(ids) == 0 {
		return
	}
	err := t.client.Post("/chat/ack", map[string]interface{}{
		"agent":       t.cfg.Agent,
		"message_ids": ids,
	}, nil)
	if err != nil {
		t.logger.Log("ERROR acking: %s", err)
	}
}

// --- Vote polling ---

func (t *Transport) checkPendingVotes() {
	var resp struct {
		OpenVotes []struct {
			ID        int    `json:"id"`
			Type      string `json:"type"`
			Threshold string `json:"threshold"`
			Question  string `json:"question"`
		} `json:"open_votes"`
	}
	err := t.client.Post("/discussion/pending", map[string]interface{}{
		"agent":         t.cfg.Agent,
		"discussion_id": t.discussionID,
	}, &resp)
	if err != nil {
		t.logger.Log("ERROR checking votes: %s", err)
		return
	}

	for _, vote := range resp.OpenVotes {
		if t.proxy.SeenVoteIDs()[vote.ID] {
			continue
		}
		t.proxy.MarkVoteSeen(vote.ID)

		notice := fmt.Sprintf("[VOTE PENDING] Vote #%d (%s, %s): %s",
			vote.ID, vote.Type, vote.Threshold, vote.Question)
		t.paths.WriteInboxFile(fmt.Sprintf("vote-pending-%d.txt", vote.ID), notice)
		t.logger.Log("VOTE NOTIFICATION: vote #%d", vote.ID)
	}
}

// --- Delayed ack ---

func (t *Transport) waitForSubagentReads(newIDs []int) {
	timeout := 120 * time.Second
	start := time.Now()
	for {
		allGone := true
		for _, id := range newIDs {
			if t.paths.InboxFileExists(fmt.Sprintf("%d.txt", id)) {
				allGone = false
				break
			}
		}

		if allGone {
			t.logger.Log("All inbox files consumed by subagent")
			return
		}

		if time.Since(start) >= timeout {
			t.logger.Log("WARNING: Delayed ack timeout (%ds) — acking unconsumed messages",
				int(timeout.Seconds()))
			return
		}

		time.Sleep(2 * time.Second)
	}
}

// --- Timeout checks ---

func (t *Transport) checkTimeout() bool {
	elapsed := time.Since(t.startTime)
	if elapsed >= time.Duration(t.cfg.TimeoutMinutes)*time.Minute {
		return true
	}
	if t.turnCount >= t.cfg.MaxMessages {
		return true
	}
	return false
}

// --- Server-side status check ---

func (t *Transport) checkServerStatus() bool {
	var resp statusResponse
	err := t.client.Post("/discussion/status", map[string]interface{}{
		"discussion_id": t.discussionID,
	}, &resp)
	if err != nil {
		t.logger.Log("Status check failed: %s", err)
		return false
	}

	status := resp.Discussion.Status
	if status == "cancelled" || status == "concluded" {
		t.logger.Log("Discussion %s server-side. Notifying subagent and shutting down.", status)
		notice := fmt.Sprintf("[SYSTEM] This discussion has been %s. Please wrap up and write your result file.", status)
		t.paths.WriteInboxFile(fmt.Sprintf("%s.txt", status), notice)
		t.paths.AppendTranscript("system", fmt.Sprintf("Discussion %s server-side", status))
		t.waitForResult()
		t.paths.WriteDone(status)
		t.setStatus("DONE")
		return true
	}

	return false
}

// --- Subagent death reporting ---

func (t *Transport) reportSubagentDead() {
	elapsed := int(time.Since(t.startTime).Minutes())
	silent := int(time.Since(t.lastOutboxTime).Minutes())
	t.logger.Log("Subagent death detected: %d messages sent, %dm elapsed, %dm silent",
		t.turnCount, elapsed, silent)

	t.client.Post("/system/error/report", map[string]interface{}{
		"source":     "discuss-transport",
		"error_code": "SUBAGENT_DEAD",
		"context": map[string]interface{}{
			"discussion_id":        t.discussionID,
			"agent":                t.cfg.Agent,
			"messages_sent":        t.turnCount,
			"elapsed_minutes":      elapsed,
			"silent_minutes":       silent,
			"last_outbox_activity": t.lastOutboxTime.UTC().Format(time.RFC3339),
			"last_inbox_delivery":  t.lastInboxDeliveryTime.UTC().Format(time.RFC3339),
		},
	}, nil)
}

// --- State management ---

func (t *Transport) setStatus(status string) {
	seenList := make([]int, 0, len(t.seenIDs))
	for id := range t.seenIDs {
		seenList = append(seenList, id)
	}

	t.paths.WriteState(&TransportState{
		Status:          status,
		PID:             os.Getpid(),
		Port:            t.proxy.Port(),
		TurnCount:       t.turnCount,
		LastDeliveredID:  t.lastDeliveredID,
		SeenIDs:         seenList,
		Ready:           t.readySignaled,
		StartedAt:       t.startTime.UTC().Format(time.RFC3339),
		Convergence:     t.convergence.Info(),
	})
}

// --- Post-transport: save transcript and result ---

// SaveTranscript copies the transcript to a persistent location and
// ingests it into vector memory.
func (t *Transport) SaveTranscript() {
	if _, err := os.Stat(t.paths.TranscriptFile); err != nil {
		return
	}

	content, err := os.ReadFile(t.paths.TranscriptFile)
	if err != nil {
		return
	}

	slug := fmt.Sprintf("notes/discussions/discussion-%d", t.discussionID)
	title := fmt.Sprintf("Discussion #%d Transcript — %s", t.discussionID, t.cfg.Topic)

	err = t.client.Post("/documents/save", map[string]interface{}{
		"namespace": t.cfg.Agent,
		"slug":      slug,
		"title":     title,
		"content":   string(content),
	}, nil)
	if err != nil {
		t.logger.Log("Transcript save failed (non-fatal): %s", err)
	} else {
		t.logger.Log("Transcript saved as remote note: %s/%s", t.cfg.Agent, slug)
	}
}

// SaveResult uploads result.md as a remote note so it's searchable
// and accessible to both agents.
func (t *Transport) SaveResult() {
	resultPath := filepath.Join(t.cfg.WorkDir, "result.md")
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.logger.Log("No result.md found — skipping result save")
		return
	}
	if strings.TrimSpace(string(content)) == "" {
		t.logger.Log("result.md is empty — skipping result save")
		return
	}

	slug := fmt.Sprintf("notes/discussions/discussion-%d-result", t.discussionID)
	title := fmt.Sprintf("Discussion #%d Result — %s", t.discussionID, t.cfg.Topic)

	err = t.client.Post("/documents/save", map[string]interface{}{
		"namespace": t.cfg.Agent,
		"slug":      slug,
		"title":     title,
		"content":   string(content),
	}, nil)
	if err != nil {
		t.logger.Log("Result save failed (non-fatal): %s", err)
	} else {
		t.logger.Log("Result saved as remote note: %s/%s", t.cfg.Agent, slug)
	}
}
