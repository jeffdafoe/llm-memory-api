package discuss

import (
	"fmt"
	"strings"
	"time"
)

// --- API response types ---

type discussionResponse struct {
	Discussion struct {
		ID        int    `json:"id"`
		Status    string `json:"status"`
		Topic     string `json:"topic"`
		Channel   string `json:"channel"`
		Context   string `json:"context"`
		Mode      string `json:"mode"`
		TimeoutAt string `json:"timeout_at"`
	} `json:"discussion"`
}

type joinResponse struct {
	DiscussionStatus string `json:"discussion_status"`
}

type pendingResponse struct {
	InvitedDiscussions []struct {
		ID     int    `json:"id"`
		Topic  string `json:"topic"`
		Status string `json:"status"`
	} `json:"invited_discussions"`
	OpenVotes []struct {
		ID        int    `json:"id"`
		Type      string `json:"type"`
		Threshold string `json:"threshold"`
		Question  string `json:"question"`
	} `json:"open_votes"`
}

type statusResponse struct {
	Discussion struct {
		ID      int    `json:"id"`
		Status  string `json:"status"`
		Topic   string `json:"topic"`
		Channel string `json:"channel"`
		Context string `json:"context"`
		Mode    string `json:"mode"`
	} `json:"discussion"`
	Participants []struct {
		Agent  string `json:"agent"`
		Status string `json:"status"`
	} `json:"participants"`
}

// SetupCreate creates a new discussion and returns the discussion ID.
// If the server returns a conflict (agent already in a discussion),
// it attempts to resolve it by joining or leaving the stale discussion.
func SetupCreate(client *APIClient, cfg *Config, logger *Logger) (int, error) {
	if err := cfg.LoadContext(); err != nil {
		return 0, err
	}

	participants := append([]string{cfg.Agent}, cfg.Others...)

	createBody := map[string]interface{}{
		"topic":        cfg.Topic,
		"participants": participants,
		"created_by":   cfg.Agent,
		"mode":         cfg.Mode,
	}
	if cfg.Context != "" {
		createBody["context"] = cfg.Context
	}
	if len(cfg.OptionalParticipants) > 0 {
		createBody["optional_participants"] = cfg.OptionalParticipants
	}

	var resp discussionResponse
	err := client.Post("/discussion/create", createBody, &resp)

	if err != nil {
		// Check for DISCUSSION_CONFLICT — agent is already in another discussion
		if !strings.Contains(err.Error(), "DISCUSSION_CONFLICT") {
			return 0, fmt.Errorf("create discussion: %w", err)
		}

		return handleConflict(client, cfg, logger, err, createBody)
	}

	logger.Log("Created discussion #%d: %s (status: %s)",
		resp.Discussion.ID, cfg.Topic, resp.Discussion.Status)
	logger.Log("Discussion ID: %d, timeout_at: %s",
		resp.Discussion.ID, resp.Discussion.TimeoutAt)

	return resp.Discussion.ID, nil
}

// handleConflict resolves a DISCUSSION_CONFLICT by checking the stale
// discussion's status and either joining it or leaving it and retrying.
func handleConflict(client *APIClient, cfg *Config, logger *Logger, conflictErr error, createBody map[string]interface{}) (int, error) {
	// Extract discussion ID from error message
	msg := conflictErr.Error()
	var staleID int
	if _, err := fmt.Sscanf(extractAfter(msg, "discussion #"), "%d", &staleID); err != nil || staleID == 0 {
		return 0, fmt.Errorf("create discussion: %w", conflictErr)
	}

	logger.Log("Create rejected — already in discussion #%d", staleID)

	// Check the stale discussion's status
	var statusResp statusResponse
	var staleStatus string
	if err := client.Post("/discussion/status", map[string]interface{}{
		"discussion_id": staleID,
	}, &statusResp); err != nil {
		logger.Log("Could not check status of #%d: %s", staleID, err)
	} else {
		staleStatus = statusResp.Discussion.Status
		logger.Log("Discussion #%d status: %s", staleID, staleStatus)
	}

	// If the stale discussion is dead, leave and retry
	if staleStatus != "" && staleStatus != "waiting" && staleStatus != "active" {
		logger.Log("Discussion #%d is %s — leaving and retrying create", staleID, staleStatus)
		leaveDiscussion(client, staleID, cfg.Agent, logger)
	} else {
		// Still active/waiting — try to join
		logger.Log("Discussion #%d is still %s — trying to join", staleID, staleStatus)
		id, err := SetupJoin(client, cfg, logger, staleID)
		if err == nil {
			return id, nil
		}
		logger.Log("Join #%d failed (%s), leaving and retrying create", staleID, err)
		leaveDiscussion(client, staleID, cfg.Agent, logger)
	}

	// Retry create after clearing
	var resp discussionResponse
	if err := client.Post("/discussion/create", createBody, &resp); err != nil {
		return 0, fmt.Errorf("retry create after clearing #%d: %w", staleID, err)
	}

	logger.Log("Created discussion #%d (after clearing stale #%d)", resp.Discussion.ID, staleID)
	return resp.Discussion.ID, nil
}

// SetupJoin joins an existing discussion and populates cfg with the
// discussion's details.
func SetupJoin(client *APIClient, cfg *Config, logger *Logger, discussionID int) (int, error) {
	// Fetch discussion details
	var statusResp statusResponse
	if err := client.Post("/discussion/status", map[string]interface{}{
		"discussion_id": discussionID,
	}, &statusResp); err != nil {
		return 0, fmt.Errorf("get discussion status: %w", err)
	}

	cfg.Topic = statusResp.Discussion.Topic
	if statusResp.Discussion.Context != "" && cfg.Context == "" {
		cfg.Context = statusResp.Discussion.Context
	}
	if statusResp.Discussion.Mode != "" {
		cfg.Mode = statusResp.Discussion.Mode
	}

	// Load context file if specified (overrides server context)
	if err := cfg.LoadContext(); err != nil {
		return 0, err
	}

	// Find other participants
	var others []string
	for _, p := range statusResp.Participants {
		if p.Agent != cfg.Agent {
			others = append(others, p.Agent)
		}
	}
	cfg.Others = others

	// Join the discussion
	var joinResp joinResponse
	if err := client.Post("/discussion/join", map[string]interface{}{
		"discussion_id": discussionID,
		"agent":         cfg.Agent,
	}, &joinResp); err != nil {
		return 0, fmt.Errorf("join discussion: %w", err)
	}

	logger.Log("Joined discussion #%d: %s (status: %s)",
		discussionID, cfg.Topic, joinResp.DiscussionStatus)
	logger.Log("Discussion ID: %d, Others: %s",
		discussionID, strings.Join(others, ", "))

	return discussionID, nil
}

// AutoDiscoverDiscussion polls for pending invitations until one is found
// or the timeout expires. Returns the discussion ID.
func AutoDiscoverDiscussion(client *APIClient, cfg *Config, logger *Logger) (int, error) {
	pollInterval := 5 * time.Second
	maxAttempts := int(time.Duration(cfg.JoinTimeout) * time.Second / pollInterval)
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var pending pendingResponse
		if err := client.Post("/discussion/pending", map[string]interface{}{
			"agent": cfg.Agent,
		}, &pending); err != nil {
			return 0, fmt.Errorf("check pending: %w", err)
		}

		if len(pending.InvitedDiscussions) > 0 {
			if len(pending.InvitedDiscussions) > 1 {
				logger.Log("Multiple pending invitations:")
				for _, inv := range pending.InvitedDiscussions {
					logger.Log("  #%d: %s (%s)", inv.ID, inv.Topic, inv.Status)
				}
				return 0, fmt.Errorf("multiple pending invitations — specify a discussion ID")
			}
			logger.Log("Auto-discovered pending invitation: discussion #%d",
				pending.InvitedDiscussions[0].ID)
			return pending.InvitedDiscussions[0].ID, nil
		}

		if attempt == maxAttempts {
			return 0, fmt.Errorf("no pending invitations after %ds", cfg.JoinTimeout)
		}

		logger.Log("No invitations yet, retrying (%d/%d)...", attempt, maxAttempts)
		time.Sleep(pollInterval)
	}

	return 0, fmt.Errorf("no pending invitations")
}

// WaitForReady polls the discussion status until it transitions from
// "waiting" to "active" (all participants joined).
func WaitForReady(client *APIClient, discussionID int, cfg *Config, logger *Logger) error {
	var statusResp statusResponse
	if err := client.Post("/discussion/status", map[string]interface{}{
		"discussion_id": discussionID,
	}, &statusResp); err != nil {
		return fmt.Errorf("check discussion status: %w", err)
	}

	if statusResp.Discussion.Status == "active" {
		logger.Log("Discussion is already active")
		return nil
	}
	if statusResp.Discussion.Status != "waiting" {
		return fmt.Errorf("discussion is %s — cannot start transport", statusResp.Discussion.Status)
	}

	logger.Log("Discussion is waiting for participants...")

	for {
		time.Sleep(cfg.PollInterval)

		if err := client.Post("/discussion/status", map[string]interface{}{
			"discussion_id": discussionID,
		}, &statusResp); err != nil {
			logger.Log("Status check error: %s", err)
			continue
		}

		switch statusResp.Discussion.Status {
		case "active":
			logger.Log("Discussion is now active — all participants ready")
			return nil
		case "timed_out":
			return fmt.Errorf("discussion timed out waiting for required participants")
		case "concluded", "cancelled":
			return fmt.Errorf("discussion was %s before it started", statusResp.Discussion.Status)
		}
	}
}

// leaveDiscussion is a helper that leaves a discussion silently.
func leaveDiscussion(client *APIClient, id int, agent string, logger *Logger) {
	err := client.Post("/discussion/leave", map[string]interface{}{
		"discussion_id": id,
		"agent":         agent,
	}, nil)
	if err != nil {
		logger.Log("Leave #%d failed: %s", id, err)
	} else {
		logger.Log("Left stale discussion #%d", id)
	}
}

// extractAfter returns the substring after the first occurrence of prefix.
func extractAfter(s, prefix string) string {
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return ""
	}
	return s[idx+len(prefix):]
}
