package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/discussion"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// The discussion and vote tools (Node's TOOL_HANDLERS discussion_create
// through discussion_vote_status). The ids, the vote type, threshold,
// closes_at and choice reach the service as the JSON values the caller
// sent, as Node passed them, so Postgres answers a bad one with its own
// message.

// rawID is args[k] as the service's id: the caller's JSON value, absent as
// nil.
func rawID(args *jsutil.Object, k string) discussion.ID {
	return discussion.RawID(raw(args, k))
}

// modeText is `${mode || 'realtime'}`.
func modeText(mode string) string {
	if mode == "" {
		return "realtime"
	}
	return mode
}

// participantList is `participants.map(p => `${p.agent} (${p.status})`).join(', ')`.
func participantList(n int, at func(i int) (agent, status string)) string {
	parts := make([]string, n)
	for i := range parts {
		agent, status := at(i)
		parts[i] = agent + " (" + status + ")"
	}
	return strings.Join(parts, ", ")
}

func (s *Server) discussionCreate(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	creator, err := identity(args, "created_by", c)
	if err != nil {
		return "", err
	}
	// validateToolArgs has made participants an array of strings.
	participants := []string{}
	for _, p := range arrayOf(args, "participants") {
		name, _ := p.(string)
		participants = append(participants, sanitize.AgentName(name))
	}
	topic, _ := str(args, "topic")
	mode, _ := str(args, "mode")
	contextText, _ := str(args, "context")
	created, err := s.Discussions.Create(ctx, discussion.CreateInput{
		Topic: sanitize.Content(topic), CreatedBy: creator, Participants: participants,
		Mode: mode, Context: sanitize.Content(contextText),
	})
	if err != nil {
		return "", err
	}
	d := created.Discussion
	parts := participantList(len(created.Participants), func(i int) (string, string) {
		return created.Participants[i].Agent, created.Participants[i].Status
	})
	return fmt.Sprintf("Discussion #%d created [%s]: \"%s\"\nParticipants: %s\nUse discussion_id: %d with chat_send to message participants.",
		d.ID, d.Mode, d.Topic, parts, d.ID), nil
}

func (s *Server) discussionList(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	// Node passed args.agent as it was: any agent's discussions are listed.
	agentName, _ := str(args, "agent")
	list, err := s.Discussions.List(ctx, rawID(args, "status"), agentName)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "No discussions found.", nil
	}
	lines := make([]string, len(list))
	for i, d := range list {
		line := fmt.Sprintf("#%d [%s] [%s] \"%s\" (created %s)", d.ID, d.Status, modeText(d.Mode), d.Topic, jsutil.DateString(d.CreatedAt.Time, s.location()))
		if d.Outcome != nil && *d.Outcome != "" {
			line += " outcome: " + *d.Outcome
		}
		if d.Channel != nil && *d.Channel != "" {
			line += " channel: " + *d.Channel
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n"), nil
}

// voteLine is `Vote #${v.id} [${v.status}] (${v.type}, ${v.threshold}): ${v.question}`.
func voteLine(v discussion.VoteRow) string {
	return fmt.Sprintf("Vote #%d [%s] (%s, %s): %s", v.ID, v.Status, v.Type, v.Threshold, v.Question)
}

func (s *Server) discussionStatus(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	st, err := s.Discussions.Status(ctx, rawID(args, "discussion_id"))
	if err != nil {
		return "", err
	}
	d := st.Discussion
	parts := participantList(len(st.Participants), func(i int) (string, string) {
		return st.Participants[i].Agent, st.Participants[i].Status
	})
	text := fmt.Sprintf("Discussion #%d: \"%s\" [%s] [%s]\nParticipants: %s", d.ID, d.Topic, d.Status, modeText(d.Mode), parts)
	if d.Outcome != nil && *d.Outcome != "" {
		text += "\nOutcome: " + *d.Outcome
	}
	if d.Channel != nil && *d.Channel != "" {
		text += "\nChannel: " + *d.Channel
	}
	if d.Context != nil && *d.Context != "" {
		text += "\nContext: " + *d.Context
	}
	if len(st.Votes) > 0 {
		lines := make([]string, len(st.Votes))
		for i, v := range st.Votes {
			lines[i] = "  " + voteLine(v.VoteRow)
		}
		text += "\nVotes:\n" + strings.Join(lines, "\n")
	}
	return text, nil
}

func (s *Server) discussionPending(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	pending, err := s.Discussions.Pending(ctx, agentName, discussion.RawID(nil))
	if err != nil {
		return "", err
	}
	discussionLines := func(rows []discussion.DiscussionRow) string {
		lines := make([]string, len(rows))
		for i, d := range rows {
			lines[i] = fmt.Sprintf("  #%d [%s] \"%s\"", d.ID, modeText(d.Mode), d.Topic)
		}
		return strings.Join(lines, "\n")
	}
	var sections []string
	if len(pending.Invited) > 0 {
		sections = append(sections, "Pending invitations:\n"+discussionLines(pending.Invited))
	}
	if len(pending.Deferred) > 0 {
		sections = append(sections, "Deferred discussions (join when ready):\n"+discussionLines(pending.Deferred))
	}
	if len(pending.Votes) > 0 {
		lines := make([]string, len(pending.Votes))
		for i, v := range pending.Votes {
			lines[i] = fmt.Sprintf("  Vote #%d [%s] in \"%s\": %s", v.ID, modeText(v.DiscussionMode), v.DiscussionTopic, v.Question)
		}
		sections = append(sections, "Open votes awaiting your ballot:\n"+strings.Join(lines, "\n"))
	}
	if len(sections) == 0 {
		return "Nothing pending.", nil
	}
	return strings.Join(sections, "\n\n"), nil
}

func (s *Server) conclude(ctx context.Context, args *jsutil.Object, c caller, cancel bool) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	done, err := s.Discussions.Conclude(ctx, rawID(args, "discussion_id"), agentName, cancel)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Discussion #%s %s. Outcome: %s.", done.DiscussionID, done.Status, done.Outcome), nil
}

func (s *Server) discussionConclude(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	return s.conclude(ctx, args, c, false)
}

func (s *Server) discussionCancel(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	return s.conclude(ctx, args, c, true)
}

func (s *Server) discussionJoin(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	joined, err := s.Discussions.Join(ctx, rawID(args, "discussion_id"), agentName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s joined discussion #%s.", joined.Agent, joined.DiscussionID), nil
}

func (s *Server) discussionLeave(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	left, err := s.Discussions.Leave(ctx, rawID(args, "discussion_id"), agentName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s left discussion #%s.", left.Agent, left.DiscussionID), nil
}

func (s *Server) discussionDefer(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	deferred, err := s.Discussions.Defer(ctx, rawID(args, "discussion_id"), agentName)
	if err != nil {
		return "", err
	}
	count := "null"
	if deferred.DeferCount != nil {
		count = fmt.Sprint(*deferred.DeferCount)
	}
	return fmt.Sprintf("%s deferred discussion #%s. Defer count: %s. Timeout extended to %s.",
		deferred.Agent, deferred.DiscussionID, count, jsutil.DateString(deferred.TimeoutAt.Time, s.location())), nil
}

func (s *Server) voteProposeTool(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	proposer, err := identity(args, "proposed_by", c)
	if err != nil {
		return "", err
	}
	question, _ := str(args, "question")
	proposed, err := s.Discussions.Propose(ctx, discussion.ProposeInput{
		DiscussionID: rawID(args, "discussion_id"), ProposedBy: proposer, Question: sanitize.Content(question),
		Type: rawID(args, "type"), Threshold: rawID(args, "threshold"), ClosesAt: rawID(args, "closes_at"),
	})
	if err != nil {
		return "", err
	}
	v := proposed.Vote
	return fmt.Sprintf("Vote #%d proposed in discussion #%s: \"%s\" (%s, %s)", v.ID, v.DiscussionID, v.Question, v.Type, v.Threshold), nil
}

func (s *Server) voteCastTool(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	// sanitize.content(args.reason): a string is sanitized, absent stays
	// absent.
	var reason json.RawMessage
	if r, ok := str(args, "reason"); ok {
		reason = json.RawMessage(jsutil.Marshal(sanitize.Content(r)))
	}
	cast, err := s.Discussions.VoteCast(ctx, rawID(args, "vote_id"), agentName, raw(args, "choice"), reason)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Vote cast on #%s: choice %s. Vote status: %s", cast.VoteID, jsutil.NumberString(float64(cast.Choice)), cast.VoteStatus), nil
}

// errNoVote is V8's TypeError for `v.id` when the vote's proposer row is
// gone and the joined query found no row.
var errNoVote = errors.New("Cannot read properties of undefined (reading 'id')")

func (s *Server) voteStatusTool(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	st, err := s.Discussions.VoteStatus(ctx, rawID(args, "vote_id"))
	if err != nil {
		return "", err
	}
	if st.Vote == nil {
		return "", errNoVote
	}
	text := voteLine(st.Vote.VoteRow)
	if len(st.Ballots) == 0 {
		return text + "\nNo ballots cast yet.", nil
	}
	lines := make([]string, len(st.Ballots))
	for i, b := range st.Ballots {
		line := fmt.Sprintf("  %s: choice %d", b.Agent, b.Choice)
		if b.Reason != nil && *b.Reason != "" {
			line += " — " + *b.Reason
		}
		lines[i] = line
	}
	return text + "\nBallots:\n" + strings.Join(lines, "\n"), nil
}
