package discuss

// VoteStatusResponse is the response from /discussion/vote/status.
type VoteStatusResponse struct {
	Vote struct {
		ID        int    `json:"id"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		Question  string `json:"question"`
		Threshold string `json:"threshold"`
	} `json:"vote"`
	Ballots []struct {
		Agent  string `json:"agent"`
		Choice int    `json:"choice"`
		Reason string `json:"reason"`
	} `json:"ballots"`
}

// ChatMessage represents a message from the chat API.
type ChatMessage struct {
	ID        int    `json:"id"`
	FromAgent string `json:"from_agent"`
	Message   string `json:"message"`
	SentAt    string `json:"sent_at"`
}

// ChatReceiveResponse is the response from /chat/receive.
type ChatReceiveResponse struct {
	Messages []ChatMessage `json:"messages"`
}
