package discuss

import (
	"fmt"
	"strings"
	"time"
)

// Convergence states — escalating ladder from normal operation to
// forced termination when agents can't reach agreement.
const (
	StateNormal     = "normal"
	StateWarned     = "warned"
	StateForcedVote = "forced-vote"
	StateCountdown  = "countdown"
	StateTerminated = "terminated"
)

// Thresholds for convergence escalation
const (
	VoteSilenceThreshold      = 10 // exchanges without any vote triggers secondary signal
	RejectedConcludeThreshold = 2  // rejected concludes triggers escalation
)

// Convergence tracks discussion convergence state and detects when
// agents are stuck in loops or can't reach agreement.
type Convergence struct {
	State                  string
	RoundTripCount         int
	RoundTripSpeakers      map[string]bool // agents who've spoken in current round-trip
	ExchangesSinceLastVote int             // messages since last vote activity
	RejectedConcludeCount  int             // consecutive rejected conclude votes
	ExchangesSinceWarning  int             // messages since convergence warning
	ExchangesSinceForcedVote int           // messages since forced conclude vote
	ForcedVoteID           *int            // vote ID of transport-proposed conclude
	ProcessedImpasseVotes  map[int]bool    // vote IDs already surfaced as impasses

	maxRounds    int
	participants []string // all participant names
	logger       *Logger
	paths        *Paths
}

// NewConvergence creates a convergence tracker for the given participants.
func NewConvergence(participants []string, maxRounds int, logger *Logger, paths *Paths) *Convergence {
	return &Convergence{
		State:                 StateNormal,
		RoundTripSpeakers:     make(map[string]bool),
		ProcessedImpasseVotes: make(map[int]bool),
		maxRounds:             maxRounds,
		participants:          participants,
		logger:                logger,
		paths:                 paths,
	}
}

// TrackMessage is called when a message is sent or received from any agent.
func (c *Convergence) TrackMessage(speaker string) {
	c.ExchangesSinceLastVote++
	if c.State == StateWarned {
		c.ExchangesSinceWarning++
	}
	if c.State == StateCountdown {
		c.ExchangesSinceForcedVote++
	}

	// Track round-trip: completes when all participants have spoken
	c.RoundTripSpeakers[speaker] = true
	if len(c.RoundTripSpeakers) >= len(c.participants) {
		c.RoundTripCount++
		c.RoundTripSpeakers = make(map[string]bool)
		c.logger.Log("Round-trip #%d completed (max: %d)", c.RoundTripCount, c.maxRounds)
	}
}

// TrackVoteActivity resets the vote-silence counter when any vote is
// proposed or cast.
func (c *Convergence) TrackVoteActivity() {
	c.ExchangesSinceLastVote = 0
}

// TrackConcludeRejected increments the rejected conclude counter.
func (c *Convergence) TrackConcludeRejected() {
	c.RejectedConcludeCount++
	c.logger.Log("Conclude vote rejected (%d/%d)", c.RejectedConcludeCount, RejectedConcludeThreshold)
}

// CheckForImpasse detects split general votes and injects an impasse
// message telling agents to write a structured summary for the user.
func (c *Convergence) CheckForImpasse(voteResult *VoteStatusResponse) {
	if voteResult == nil || voteResult.Vote.Status != "closed" || voteResult.Vote.Type == "conclude" {
		return
	}
	if c.ProcessedImpasseVotes[voteResult.Vote.ID] {
		return
	}
	c.ProcessedImpasseVotes[voteResult.Vote.ID] = true

	// Check if the vote split
	choices := make(map[int]bool)
	for _, b := range voteResult.Ballots {
		choices[b.Choice] = true
	}
	if len(choices) <= 1 {
		return // unanimous
	}

	c.logger.Log("IMPASSE DETECTED: vote #%d split (%d different choices across %d ballots)",
		voteResult.Vote.ID, len(choices), len(voteResult.Ballots))

	var lines []string
	for _, b := range voteResult.Ballots {
		reason := ""
		if b.Reason != "" {
			reason = ": " + b.Reason
		}
		lines = append(lines, fmt.Sprintf("  - %s voted %d%s", b.Agent, b.Choice, reason))
	}

	message := fmt.Sprintf("[SYSTEM] Impasse detected: Vote #%d split — no unanimous agreement.\n%s\n\n"+
		"This disagreement will be surfaced to the user for their decision. "+
		"Before concluding, write a structured summary with:\n"+
		"1. Each position's pros and cons (balanced, not advocating)\n"+
		"2. What you agreed on\n"+
		"3. The specific point of disagreement\n"+
		"4. Your recommendation (if any) with reasoning\n\n"+
		"Then propose a conclude vote.",
		voteResult.Vote.ID, strings.Join(lines, "\n"))

	c.InjectSystemMessage(message)
}

// InjectSystemMessage writes a [SYSTEM] message into the subagent's inbox.
func (c *Convergence) InjectSystemMessage(message string) {
	filename := fmt.Sprintf("system-%d.txt", time.Now().UnixMilli())
	content := fmt.Sprintf("From: system\nSent: %s\n\n%s", time.Now().UTC().Format(time.RFC3339), message)
	c.paths.WriteInboxFile(filename, content)
	c.paths.AppendTranscript("system", message)
	c.logger.Log("SYSTEM MESSAGE INJECTED: %.100s...", message)
}

// Check evaluates convergence signals and escalates if needed.
// Returns "continue" or "terminate". The client parameter is needed
// for proposing forced votes.
func (c *Convergence) Check(client *APIClient, discussionID int, agent string) string {
	if c.State == StateTerminated {
		return "terminate"
	}
	if c.paths.IsDone() {
		return "continue"
	}

	shouldEscalate :=
		(c.State == StateNormal && c.RoundTripCount >= c.maxRounds) ||
			(c.State == StateNormal && c.RejectedConcludeCount >= RejectedConcludeThreshold) ||
			(c.State == StateNormal && c.ExchangesSinceLastVote >= VoteSilenceThreshold && c.RoundTripCount >= c.maxRounds/2)

	if shouldEscalate && c.State == StateNormal {
		return c.escalateToWarned()
	}

	if c.State == StateWarned && c.ExchangesSinceWarning >= 3 {
		return c.escalateToForcedVote(client, discussionID, agent)
	}

	if c.State == StateForcedVote && c.ForcedVoteID != nil {
		return c.checkForcedVote(client, agent)
	}

	if c.State == StateCountdown && c.ExchangesSinceForcedVote >= 2 {
		return c.escalateToTerminated()
	}

	return "continue"
}

func (c *Convergence) escalateToWarned() string {
	c.State = StateWarned
	c.ExchangesSinceWarning = 0

	var reason string
	if c.RoundTripCount >= c.maxRounds {
		reason = fmt.Sprintf("%d round-trips without resolution", c.RoundTripCount)
	} else if c.RejectedConcludeCount >= RejectedConcludeThreshold {
		reason = fmt.Sprintf("%d conclude votes rejected", c.RejectedConcludeCount)
	} else {
		reason = fmt.Sprintf("%d exchanges without a vote", c.ExchangesSinceLastVote)
	}

	warning := fmt.Sprintf("[SYSTEM] Convergence warning: This discussion has reached %s. "+
		"Please propose a vote, concede a point, or identify the exact disagreement. "+
		"Continuing to restate positions will trigger escalation.", reason)
	c.InjectSystemMessage(warning)
	c.logger.Log("CONVERGENCE: escalated to 'warned' (%s)", reason)
	return "continue"
}

func (c *Convergence) escalateToForcedVote(client *APIClient, discussionID int, agent string) string {
	c.State = StateForcedVote
	c.ExchangesSinceForcedVote = 0
	c.logger.Log("CONVERGENCE: grace period expired, proposing forced conclude vote")

	var result struct {
		Vote struct {
			ID int `json:"id"`
		} `json:"vote"`
	}
	err := client.Post("/discussion/vote/propose", map[string]interface{}{
		"discussion_id": discussionID,
		"proposed_by":   agent,
		"question":      "[SYSTEM] Forced conclude: discussion has not converged after warning. 1=conclude 2=continue",
		"type":          "conclude",
		"threshold":     "majority",
	}, &result)
	if err != nil {
		c.logger.Log("Failed to propose forced conclude: %s", err)
		c.State = StateCountdown
		c.ExchangesSinceForcedVote = 0
		return "continue"
	}

	id := result.Vote.ID
	c.ForcedVoteID = &id
	c.InjectSystemMessage(fmt.Sprintf("[SYSTEM] A forced conclude vote has been proposed (vote #%d). "+
		"Vote to conclude or continue. If this vote fails, the discussion will be terminated shortly.", id))

	return "continue"
}

func (c *Convergence) checkForcedVote(client *APIClient, agent string) string {
	var result VoteStatusResponse
	err := client.Post("/discussion/vote/status", map[string]interface{}{
		"vote_id": *c.ForcedVoteID,
	}, &result)
	if err != nil {
		c.logger.Log("Forced vote status check failed: %s", err)
	} else {
		if result.Vote.Status == "passed" {
			c.logger.Log("CONVERGENCE: forced conclude vote passed")
			return "continue"
		}
		if result.Vote.Status == "failed" {
			c.logger.Log("CONVERGENCE: forced conclude vote rejected — entering countdown")
			c.State = StateCountdown
			c.ExchangesSinceForcedVote = 0
			c.InjectSystemMessage("[SYSTEM] Final warning: The forced conclude vote was rejected. " +
				"You have 2 more exchanges to reach agreement before this discussion is terminated.")
			return "continue"
		}
	}

	// Vote still open — give it time
	if c.ExchangesSinceForcedVote >= 4 {
		c.State = StateCountdown
		c.ExchangesSinceForcedVote = 0
		c.InjectSystemMessage("[SYSTEM] Final warning: forced conclude vote timed out. " +
			"You have 2 more exchanges to reach agreement before this discussion is terminated.")
	}
	return "continue"
}

func (c *Convergence) escalateToTerminated() string {
	c.State = StateTerminated
	c.logger.Log("CONVERGENCE: terminated — discussion did not converge")
	c.InjectSystemMessage("[SYSTEM] This discussion has been terminated due to failure to converge. " +
		"Please write your result.md summarizing what was agreed and what remains unresolved.")
	return "terminate"
}

// Info returns the convergence state for the state file.
func (c *Convergence) Info() ConvergenceInfo {
	return ConvergenceInfo{
		State:                  c.State,
		RoundTrips:             c.RoundTripCount,
		MaxRounds:              c.maxRounds,
		ExchangesSinceLastVote: c.ExchangesSinceLastVote,
		RejectedConcludes:      c.RejectedConcludeCount,
	}
}
