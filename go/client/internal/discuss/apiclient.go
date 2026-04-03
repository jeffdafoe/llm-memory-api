package discuss

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIClient is an HTTP client for the memory API with session-based auth,
// automatic retry with exponential backoff, and re-login on 401/403.
// Unlike the shared api.Client (designed for short-lived sync operations),
// this client is built for long-running transport sessions where network
// hiccups and session expiry are expected.
type APIClient struct {
	baseURL    string
	agent      string
	passphrase string
	token      string
	httpClient *http.Client
	logger     *Logger
}

// HTTPError represents a non-2xx response from the API.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

const (
	maxRetries   = 3
	baseDelayMs  = 1000
)

// retryable status codes — server-side issues worth retrying
var retryableStatus = map[int]bool{502: true, 503: true, 504: true}

// retryable network error substrings
var retryableErrors = []string{
	"connection reset",
	"connection refused",
	"i/o timeout",
	"no such host",
	"temporary failure",
	"broken pipe",
}

// NewAPIClient creates a client but does not log in yet.
// Call Login() before making authenticated requests.
func NewAPIClient(baseURL, agent, passphrase string, logger *Logger) *APIClient {
	return &APIClient{
		baseURL:    baseURL,
		agent:      agent,
		passphrase: passphrase,
		logger:     logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Login authenticates with the API and stores the session token.
func (c *APIClient) Login() error {
	var resp struct {
		SessionToken string `json:"session_token"`
		ExpiresAt    string `json:"expires_at"`
	}
	err := c.postOnce("/agent/login", map[string]string{
		"agent":      c.agent,
		"passphrase": c.passphrase,
		"subsystem":  "discussion",
	}, &resp)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	if resp.SessionToken == "" {
		return fmt.Errorf("login failed: empty session_token")
	}
	c.token = resp.SessionToken
	c.logger.Log("Logged in as %s (session expires %s)", c.agent, resp.ExpiresAt)
	return nil
}

// Logout ends the API session. Non-fatal if it fails.
func (c *APIClient) Logout() {
	if c.token == "" {
		return
	}
	c.Post("/agent/logout", map[string]string{"agent": c.agent}, nil)
	c.logger.Log("Logged out")
	c.token = ""
}

// Post sends an authenticated JSON POST with retry logic.
// On 401/403, re-logs in and retries once.
func (c *APIClient) Post(path string, body interface{}, result interface{}) error {
	if c.token == "" {
		return fmt.Errorf("not logged in — no session token")
	}

	err := c.postWithRetry(path, body, result)
	if err == nil {
		return nil
	}

	// Check for auth failure — re-login and retry once
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == 401 || httpErr.StatusCode == 403 {
			c.logger.Log("Session expired or invalid — re-logging in")
			if loginErr := c.Login(); loginErr != nil {
				return fmt.Errorf("re-login failed: %w", loginErr)
			}
			return c.postWithRetry(path, body, result)
		}
	}

	return err
}

// PostNoAuth sends a JSON POST without authentication. Used for login
// and health checks.
func (c *APIClient) PostNoAuth(path string, body interface{}, result interface{}) error {
	return c.postOnce(path, body, result)
}

// Get sends a simple GET request. Used for health checks.
func (c *APIClient) Get(url string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return nil
}

// postWithRetry wraps postOnce with exponential backoff retries for
// transient network errors and 5xx responses.
func (c *APIClient) postWithRetry(path string, body interface{}, result interface{}) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lastErr = c.postAuthOnce(path, body, result)
		if lastErr == nil {
			return nil
		}
		if attempt < maxRetries && isRetryable(lastErr) {
			delay := time.Duration(baseDelayMs*(1<<attempt)) * time.Millisecond
			c.logger.Log("Retryable error (attempt %d/%d): %s — retrying in %v",
				attempt+1, maxRetries+1, lastErr, delay)
			time.Sleep(delay)
		}
	}
	return lastErr
}

// postAuthOnce sends a single authenticated POST request.
func (c *APIClient) postAuthOnce(path string, body interface{}, result interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
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

// postOnce sends a single unauthenticated POST request.
func (c *APIClient) postOnce(path string, body interface{}, result interface{}) error {
	savedToken := c.token
	c.token = ""
	err := c.postAuthOnce(path, body, result)
	c.token = savedToken
	return err
}

// isRetryable returns true if the error is a transient network issue
// or server error worth retrying.
func isRetryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return retryableStatus[httpErr.StatusCode]
	}

	msg := strings.ToLower(err.Error())
	for _, substr := range retryableErrors {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}
