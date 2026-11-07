// Package mcp is the /mcp endpoint: the Model Context Protocol over
// streamable HTTP (Node's routes/mcp.js on the v2 TypeScript SDK). Every
// request is answered on its own, with no session, for both protocol eras:
// 2026-07-28 clients (server/discover, then a _meta envelope on each request)
// and 2025-era clients (the initialize handshake). The tools are the memory
// API's notes, search, chat, mail, agent and discussion operations, gated by
// the agent's mcp_* permissions.
package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/discussion"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/signup"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// Server holds what the tools call.
type Server struct {
	DB          db.DB
	Config      *config.Store
	Log         *logger.Logger
	Events      events.Broadcaster
	Namespaces  *permissions.Namespaces
	Shares      *permissions.NoteShares
	Notes       *notes.Service
	Memory      *memory.Service
	Chat        *chat.Service
	Mail        *mail.Service
	Visibility  *visibility.Visibility
	Discussions *discussion.Service
	// Location is the zone dates are printed in (Date.prototype.toString);
	// nil is time.Local, the process zone, as in Node.
	Location *time.Location
	// Stdout takes the connect log lines (Node's console.log); nil is
	// os.Stdout.
	Stdout io.Writer

	// callsDone, for tests, runs once every tool call of a 2025-era POST has
	// finished and its worker has exited.
	callsDone func()
}

// caller is the authenticated agent of one request, as mcpAuth found it.
type caller struct {
	agent   string
	actorID int
	// namespace is the default namespace of the tools: the agent's own.
	namespace   string
	permissions []string
}

type toolHandler func(s *Server, ctx context.Context, args *jsutil.Object, c caller) (string, error)

// toolHandlers is Node's TOOL_HANDLERS for the tools ported so far.
var toolHandlers = map[string]toolHandler{
	"search":                  (*Server).search,
	"save_note":               (*Server).saveNote,
	"list_notes":              (*Server).listNotes,
	"read_note":               (*Server).readNote,
	"delete_note":             (*Server).deleteNote,
	"restore_note":            (*Server).restoreNote,
	"edit_note":               (*Server).editNote,
	"move_note":               (*Server).moveNote,
	"move_folder":             (*Server).moveFolder,
	"grep":                    (*Server).grep,
	"read_instructions":       (*Server).readInstructions,
	"save_instructions":       (*Server).saveInstructions,
	"chat_send":               (*Server).chatSend,
	"chat_receive":            (*Server).chatReceive,
	"chat_ack":                (*Server).chatAck,
	"chat_status":             (*Server).chatStatus,
	"mail_send":               (*Server).mailSend,
	"mail_check":              (*Server).mailCheck,
	"mail_receive":            (*Server).mailReceive,
	"mail_ack":                (*Server).mailAck,
	"mail_edit":               (*Server).mailEdit,
	"mail_unsend":             (*Server).mailUnsend,
	"mail_sent":               (*Server).mailSent,
	"mail_history":            (*Server).mailHistory,
	"agent_status":            (*Server).agentStatus,
	"update_expertise":        (*Server).updateExpertise,
	"update_profile":          (*Server).updateProfile,
	"activity_start":          (*Server).activityStart,
	"activity_stop":           (*Server).activityStop,
	"discussion_create":       (*Server).discussionCreate,
	"discussion_list":         (*Server).discussionList,
	"discussion_status":       (*Server).discussionStatus,
	"discussion_pending":      (*Server).discussionPending,
	"discussion_conclude":     (*Server).discussionConclude,
	"discussion_cancel":       (*Server).discussionCancel,
	"discussion_join":         (*Server).discussionJoin,
	"discussion_leave":        (*Server).discussionLeave,
	"discussion_defer":        (*Server).discussionDefer,
	"discussion_vote_propose": (*Server).voteProposeTool,
	"discussion_vote_cast":    (*Server).voteCastTool,
	"discussion_vote_status":  (*Server).voteStatusTool,
}

// toolResult is a CallToolResult with one text block.
type toolResult struct {
	text    string
	isError bool
}

func errorResult(text string) toolResult {
	return toolResult{text: text, isError: true}
}

// callTool is the tools/call handler of createMcpServer: the permission
// check, the signup-funnel stamp, argument validation, then the tool. A
// refusal or a tool's error is an error result, never a JSON-RPC error; a
// tool error is logged with its status, so the dashboard tells a client's
// 4xx from a failure.
//
// The error is one Node's handler threw outside its try (the signup stamp
// reading a missing config key), which the SDK answers as a JSON-RPC error.
func (s *Server) callTool(ctx context.Context, c caller, name string, args any) (toolResult, error) {
	perm, known := toolPermission(name)
	if !known {
		return errorResult("Unknown tool: " + name), nil
	}
	if !perm.held(c.permissions) {
		return errorResult("Permission denied: " + perm.text), nil
	}
	handler, ok := toolHandlers[name]
	if !ok {
		return errorResult("No handler for tool: " + name), nil
	}

	// Signup funnel (LLM-734): a real tool call counts as use, stamped before
	// the tool runs so one that fails still counts.
	if err := s.stampToolCall(c.actorID); err != nil {
		return toolResult{}, err
	}

	text, err := s.runTool(ctx, handler, name, args, c)
	if err != nil {
		status, _ := apierr.StatusAndCode(err)
		message := httpapi.ErrorMessage(err)
		s.Log.LogError("mcp", "tool-"+name, logger.ErrorOpts{
			Agent:      c.agent,
			Message:    message,
			Detail:     fmt.Sprintf("%+v", err),
			StatusCode: status,
		})
		return errorResult("Error: " + message), nil
	}
	return toolResult{text: text}, nil
}

// runTool validates the arguments and runs the tool; a panic is an error,
// as a thrown TypeError was in Node.
func (s *Server) runTool(ctx context.Context, handler toolHandler, name string, args any, c caller) (text string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	valid, err := validateToolArgs(name, args)
	if err != nil {
		return "", err
	}
	return handler(s, ctx, valid, c)
}

// stampToolCall is signup-funnel's stampToolCall: the account's first and
// last tool call and its count, in the background. A failed write goes to
// stderr; a missing signup_funnel_since key is returned, as Node's config
// read threw before the query.
func (s *Server) stampToolCall(actorID int) error {
	if actorID == 0 {
		return nil
	}
	since, err := signup.TrackingSince(s.Config)
	if err != nil {
		return err
	}
	db.Background(func(ctx context.Context) {
		_, err := s.DB.Exec(ctx, `UPDATE actors
         SET first_tool_call_at = COALESCE(first_tool_call_at,
                 CASE WHEN created_at >= $2::timestamptz THEN NOW() END),
             last_tool_call_at = NOW(),
             tool_call_count = tool_call_count + 1
         WHERE id = $1`, actorID, since)
		if err != nil {
			s.Log.Errorf("signup-funnel: tool-call stamp failed: %s", httpapi.ErrorMessage(err))
		}
	})
	return nil
}

func (s *Server) stdout() io.Writer {
	if s.Stdout != nil {
		return s.Stdout
	}
	return os.Stdout
}

// decodeNameHeader reads an Mcp-Name header: a "=?base64?...?=" sentinel is
// decoded (strict padding, valid UTF-8), anything else is the name itself.
func decodeNameHeader(v string) (string, bool) {
	if !strings.HasPrefix(v, "=?base64?") || !strings.HasSuffix(v, "?=") {
		return v, true
	}
	// "=?base64?=" overlaps prefix and suffix; JavaScript's slice gives "".
	inner := ""
	if len(v) >= len("=?base64??=") {
		inner = v[len("=?base64?") : len(v)-len("?=")]
	}
	if !base64Sentinel.MatchString(inner) {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(inner)
	if err != nil || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

var base64Sentinel = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)
