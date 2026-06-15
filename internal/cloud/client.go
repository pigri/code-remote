// Package cloud talks to the Anthropic Sessions API — the server-side source of
// truth for Remote Control session state (archived / connection status), which
// is NOT mirrored into ~/.claude on the host running the screens.
//
// Endpoints (from the claude-code Remote Control session API):
//
//	GET    /v1/sessions            list sessions
//	POST   /v1/sessions/{id}/archive
//	DELETE /v1/sessions/{id}
//
// Auth is the local OAuth access token (the same one claude itself uses),
// read from ~/.claude/.credentials.json. The token is never logged.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Anthropic API host.
	DefaultBaseURL = "https://api.anthropic.com"
	betaHeader     = "ccr-byoc-2025-07-29"
	apiVersion     = "2023-06-01"
)

// Session is one Remote Control session as reported by the server.
//
// The server id is a bridge id (e.g. "session_017RC...") — NOT the claude
// session UUID we assign with --session-id (that UUID does not appear in the
// API payload). So local screens are joined to these by Title + Cwd (and, when
// available, the registry's bridgeSessionId == ID). Archive state lives in the
// string SessionStatus ("archived"), not a boolean.
type Session struct {
	ID               string `json:"id"`             // server/bridge session id
	Title            string `json:"title"`          // user-set display name (== custom-title)
	SessionStatus    string `json:"session_status"` // archived | idle | running | pending | requires_action
	ConnectionStatus string `json:"connection_status"`
	SessionContext   struct {
		Cwd string `json:"cwd"`
	} `json:"session_context"`
}

// Cwd is the session's working directory (from session_context).
func (s Session) Cwd() string { return s.SessionContext.Cwd }

// IsArchivedSession reports whether the server considers this session archived.
func (s Session) IsArchivedSession() bool {
	return strings.EqualFold(strings.TrimSpace(s.SessionStatus), "archived")
}

// Client is a minimal Anthropic Sessions API client.
type Client struct {
	BaseURL         string // default DefaultBaseURL
	CredentialsPath string // default ~/.claude/.credentials.json
	HTTP            *http.Client
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// List returns the caller's Remote Control sessions.
func (c *Client) List(ctx context.Context) ([]Session, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/sessions?limit=100", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []Session `json:"data"`
	}
	if err := c.do(req, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// Archive marks a session archived on the server.
func (c *Client) Archive(ctx context.Context, id string) error {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/sessions/"+id+"/archive", nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	token, err := loadToken(c.CredentialsPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", betaHeader)
	req.Header.Set("anthropic-version", apiVersion)
	return req, nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a little of the body for context; it carries an API error
		// message, not the request token.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sessions API %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// loadToken reads the OAuth access token from the credentials file. It accepts
// the nested {"claudeAiOauth":{"accessToken":...}} layout claude writes, with a
// fallback to a top-level accessToken.
func loadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read credentials: %w", err)
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse credentials: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken != "" {
		return creds.ClaudeAiOauth.AccessToken, nil
	}
	if creds.AccessToken != "" {
		return creds.AccessToken, nil
	}
	return "", fmt.Errorf("no OAuth access token in %s", path)
}
