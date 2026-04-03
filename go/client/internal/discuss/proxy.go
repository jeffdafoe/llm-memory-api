package discuss

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ProxyServer is a local HTTP server that proxies discussion-related
// API calls from the Claude subagent to the remote memory API. This
// lets the subagent call endpoints like /vote/propose without needing
// credentials — the proxy handles authentication.
type ProxyServer struct {
	server       *http.Server
	listener     net.Listener
	port         int
	client       *APIClient
	discussionID int
	agent        string
	convergence  *Convergence
	paths        *Paths
	logger       *Logger
	seenVoteIDs  map[int]bool
	startTime    time.Time
}

// NewProxyServer creates a proxy server bound to localhost on a random port.
func NewProxyServer(client *APIClient, discussionID int, agent string,
	convergence *Convergence, paths *Paths, logger *Logger, startTime time.Time) (*ProxyServer, error) {

	p := &ProxyServer{
		client:       client,
		discussionID: discussionID,
		agent:        agent,
		convergence:  convergence,
		paths:        paths,
		logger:       logger,
		seenVoteIDs:  make(map[int]bool),
		startTime:    startTime,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/vote/propose", p.handleVotePropose)
	mux.HandleFunc("/vote/cast", p.handleVoteCast)
	mux.HandleFunc("/vote/status", p.handleVoteStatus)
	mux.HandleFunc("/pending", p.handlePending)
	mux.HandleFunc("/conclude", p.handleConclude)
	mux.HandleFunc("/status", p.handleStatus)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	p.port = listener.Addr().(*net.TCPAddr).Port
	p.listener = listener
	p.server = &http.Server{Handler: mux}

	return p, nil
}

// Start begins serving in a background goroutine.
func (p *ProxyServer) Start() {
	go p.server.Serve(p.listener)
	p.logger.Log("Proxy server listening on 127.0.0.1:%d", p.port)
}

// Port returns the port the server is listening on.
func (p *ProxyServer) Port() int {
	return p.port
}

// Close shuts down the proxy server.
func (p *ProxyServer) Close() {
	p.server.Close()
}

// MarkVoteSeen records a vote ID so checkPendingVotes doesn't re-notify.
func (p *ProxyServer) MarkVoteSeen(id int) {
	p.seenVoteIDs[id] = true
}

// SeenVoteIDs returns the set of vote IDs already seen by the proxy.
func (p *ProxyServer) SeenVoteIDs() map[int]bool {
	return p.seenVoteIDs
}

// --- Handlers ---

func (p *ProxyServer) handleVotePropose(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		p.writeError(w, 405, "method not allowed")
		return
	}

	var req struct {
		Question  string `json:"question"`
		Type      string `json:"type"`
		Threshold string `json:"threshold"`
	}
	if err := p.readBody(r, &req); err != nil {
		p.writeError(w, 400, err.Error())
		return
	}

	p.convergence.TrackVoteActivity()

	voteType := req.Type
	if voteType == "" {
		voteType = "general"
	}
	threshold := req.Threshold
	if threshold == "" {
		threshold = "unanimous"
	}

	// If proposing a conclude vote, check for an existing one first
	if voteType == "conclude" {
		var pending pendingResponse
		if err := p.client.Post("/discussion/pending", map[string]interface{}{
			"agent":         p.agent,
			"discussion_id": p.discussionID,
		}, &pending); err == nil {
			for _, v := range pending.OpenVotes {
				if v.Type == "conclude" {
					p.logger.Log("Found existing conclude vote #%d, casting yes instead of proposing", v.ID)
					var result interface{}
					err := p.client.Post("/discussion/vote/cast", map[string]interface{}{
						"vote_id": v.ID,
						"agent":   p.agent,
						"choice":  1,
						"reason":  "Auto-agreed to existing conclude vote",
					}, &result)
					if err != nil {
						p.writeError(w, 502, err.Error())
						return
					}
					p.seenVoteIDs[v.ID] = true
					p.writeJSON(w, result)
					return
				}
			}
		}
	}

	var result interface{}
	err := p.client.Post("/discussion/vote/propose", map[string]interface{}{
		"discussion_id": p.discussionID,
		"proposed_by":   p.agent,
		"question":      req.Question,
		"type":          voteType,
		"threshold":     threshold,
	}, &result)
	if err != nil {
		p.writeError(w, 502, err.Error())
		return
	}
	p.writeJSON(w, result)
}

func (p *ProxyServer) handleVoteCast(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		p.writeError(w, 405, "method not allowed")
		return
	}

	var req struct {
		VoteID int    `json:"vote_id"`
		Choice int    `json:"choice"`
		Reason string `json:"reason"`
	}
	if err := p.readBody(r, &req); err != nil {
		p.writeError(w, 400, err.Error())
		return
	}

	p.convergence.TrackVoteActivity()

	body := map[string]interface{}{
		"vote_id": req.VoteID,
		"agent":   p.agent,
		"choice":  req.Choice,
	}
	if req.Reason != "" {
		body["reason"] = req.Reason
	}

	var result interface{}
	err := p.client.Post("/discussion/vote/cast", body, &result)
	if err != nil {
		p.writeError(w, 502, err.Error())
		return
	}

	// Append to transcript
	reasonText := ""
	if req.Reason != "" {
		reasonText = fmt.Sprintf(" (%s)", req.Reason)
	}
	p.paths.AppendTranscript("system",
		fmt.Sprintf("%s voted %d on vote #%d%s", p.agent, req.Choice, req.VoteID, reasonText))

	p.writeJSON(w, result)
}

func (p *ProxyServer) handleVoteStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		p.writeError(w, 405, "method not allowed")
		return
	}

	var req struct {
		VoteID int `json:"vote_id"`
	}
	if err := p.readBody(r, &req); err != nil {
		p.writeError(w, 400, err.Error())
		return
	}

	var result VoteStatusResponse
	err := p.client.Post("/discussion/vote/status", map[string]interface{}{
		"vote_id": req.VoteID,
	}, &result)
	if err != nil {
		p.writeError(w, 502, err.Error())
		return
	}

	// Track rejected conclude votes for convergence detection
	if result.Vote.Type == "conclude" && result.Vote.Status == "closed" {
		choices := make(map[int]bool)
		for _, b := range result.Ballots {
			choices[b.Choice] = true
		}
		if len(choices) > 1 {
			p.convergence.TrackConcludeRejected()
		}
	}

	// Detect split general votes and surface impasse
	p.convergence.CheckForImpasse(&result)

	p.writeJSON(w, result)
}

func (p *ProxyServer) handlePending(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		p.writeError(w, 405, "method not allowed")
		return
	}

	var result interface{}
	err := p.client.Post("/discussion/pending", map[string]interface{}{
		"agent": p.agent,
	}, &result)
	if err != nil {
		p.writeError(w, 502, err.Error())
		return
	}
	p.writeJSON(w, result)
}

func (p *ProxyServer) handleConclude(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		p.writeError(w, 405, "method not allowed")
		return
	}

	p.paths.AppendTranscript("system", fmt.Sprintf("%s concluded the discussion", p.agent))

	var result interface{}
	err := p.client.Post("/discussion/conclude", map[string]interface{}{
		"discussion_id": p.discussionID,
		"agent":         p.agent,
	}, &result)
	if err != nil {
		p.writeError(w, 502, err.Error())
		return
	}
	p.writeJSON(w, result)
}

func (p *ProxyServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		p.writeError(w, 405, "method not allowed")
		return
	}

	state := p.paths.ReadState()
	response := map[string]interface{}{
		"agent":          p.agent,
		"discussionId":   p.discussionID,
		"elapsedMinutes": int(time.Since(p.startTime).Minutes()),
	}
	if state != nil {
		response["status"] = state.Status
		response["turnCount"] = state.TurnCount
		response["lastDeliveredId"] = state.LastDeliveredID
		response["convergence"] = state.Convergence
	}
	p.writeJSON(w, response)
}

// --- Helpers ---

func (p *ProxyServer) readBody(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

func (p *ProxyServer) writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (p *ProxyServer) writeError(w http.ResponseWriter, status int, message string) {
	p.logger.Log("Proxy error: %s", message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
