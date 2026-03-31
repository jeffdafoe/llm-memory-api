// Package api provides an HTTP client for the memory API. Handles
// authentication (login/logout) and provides a generic JSON POST helper
// that all sync operations use. The client manages a session token
// obtained at login and passes it as a Bearer header on subsequent requests.
package api

import (
    "bytes"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "time"

    "github.com/jeffdafoe/llm-memory-api/go/client/internal/config"
)

// HTTPError represents a non-2xx response from the API.
type HTTPError struct {
    StatusCode int
    Body       string
}

func (e *HTTPError) Error() string {
    return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// IsNotFound returns true if the error is an HTTP 404.
func IsNotFound(err error) bool {
    var httpErr *HTTPError
    if errors.As(err, &httpErr) {
        return httpErr.StatusCode == 404
    }
    return false
}

// Client wraps an authenticated session with the memory API.
type Client struct {
    baseURL    string
    token      string
    agent      string
    httpClient *http.Client
}

// loginResponse is the shape of the /agent/login response.
type loginResponse struct {
    SessionToken string `json:"session_token"`
}

// New creates a Client and logs in to obtain a session token.
// The caller should defer client.Logout() to clean up the session.
func New(cfg *config.AgentConfig) (*Client, error) {
    c := &Client{
        baseURL: cfg.APIURL,
        agent:   cfg.Agent,
        httpClient: &http.Client{
            Timeout: 30 * time.Second,
            // Disable redirects so Authorization headers are never sent
            // to a different host if the server issues a redirect.
            CheckRedirect: func(req *http.Request, via []*http.Request) error {
                return http.ErrUseLastResponse
            },
        },
    }

    // Log in
    var resp loginResponse
    err := c.postJSON("/agent/login", map[string]string{
        "agent":      cfg.Agent,
        "passphrase": cfg.Passphrase,
        "subsystem":  "memory-sync",
    }, &resp)
    if err != nil {
        return nil, fmt.Errorf("login failed: %w", err)
    }
    if resp.SessionToken == "" {
        return nil, fmt.Errorf("login failed: empty session_token")
    }
    c.token = resp.SessionToken

    return c, nil
}

// Logout ends the API session. Non-fatal if it fails — the session
// will expire on its own.
func (c *Client) Logout() {
    _ = c.Post("/agent/logout", map[string]string{
        "agent": c.agent,
    }, nil)
}

// Post sends a JSON POST request to the given API path and decodes
// the response into result (if non-nil). The path is relative to the
// base URL (e.g., "/agent/memory/sync").
func (c *Client) Post(path string, body interface{}, result interface{}) error {
    return c.postJSON(path, body, result)
}

// postJSON is the internal POST helper. Before login, it sends requests
// without auth headers; after login, it includes the Bearer token.
func (c *Client) postJSON(path string, body interface{}, result interface{}) error {
    data, err := json.Marshal(body)
    if err != nil {
        return fmt.Errorf("marshal request body: %w", err)
    }

    url := c.baseURL + path
    req, err := http.NewRequest("POST", url, bytes.NewReader(data))
    if err != nil {
        return fmt.Errorf("create request: %w", err)
    }

    req.Header.Set("Content-Type", "application/json")
    if c.token != "" {
        req.Header.Set("Authorization", "Bearer "+c.token)
    }

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("request to %s: %w", path, err)
    }
    defer resp.Body.Close()

    respBody, err := io.ReadAll(resp.Body)
    if err != nil {
        return fmt.Errorf("read response from %s: %w", path, err)
    }

    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return &HTTPError{StatusCode: resp.StatusCode, Body: string(respBody)}
    }

    if result != nil {
        if err := json.Unmarshal(respBody, result); err != nil {
            return fmt.Errorf("decode response from %s: %w", path, err)
        }
    }

    return nil
}
