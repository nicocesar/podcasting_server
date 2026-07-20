package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// API is the slice of the Claude Managed Agents API the pipeline needs.
// The tests substitute a fake; production uses Client.
type API interface {
	// EnsureAgent creates the named agent or brings its configuration up
	// to date (a changed system prompt or toolset becomes a new agent
	// version), and returns the agent ID.
	EnsureAgent(ctx context.Context, name, model, system string, tools []map[string]any) (string, error)
	// EnsureEnvironment creates the named cloud environment if missing
	// and returns its ID.
	EnsureEnvironment(ctx context.Context, name string) (string, error)
	CreateSession(ctx context.Context, agentID, envID, title string) (string, error)
	SendMessage(ctx context.Context, sessionID, text string) error
	// SessionStatus is one of idle, running, rescheduling, terminated.
	SessionStatus(ctx context.Context, sessionID string) (string, error)
	// SessionUsage is the session's aggregate token consumption so far.
	SessionUsage(ctx context.Context, sessionID string) (Usage, error)
	// LastAgentMessage returns the text of the most recent agent message
	// in the session's event history ("" when the agent has not spoken).
	LastAgentMessage(ctx context.Context, sessionID string) (string, error)
	// LastToolUse returns the most recent call of the named custom tool
	// in the session's event history (nil when there is none).
	LastToolUse(ctx context.Context, sessionID, name string) (*ToolUse, error)
	// SendToolResult answers a custom tool call, unblocking the session.
	SendToolResult(ctx context.Context, sessionID, toolUseID, text string, isError bool) error
	DeleteSession(ctx context.Context, sessionID string) error
	// CompleteJSON runs one plain /v1/messages call whose output is
	// constrained to the given JSON schema, and returns the raw JSON
	// text. Used for small non-agentic steps like character extraction.
	CompleteJSON(ctx context.Context, model, prompt string, schema map[string]any, maxTokens int) (string, Usage, error)
}

// ToolUse is one custom tool call from the event stream. Answered means
// a tool result for it was already sent (a session accepts exactly one).
type ToolUse struct {
	ID       string
	Input    json.RawMessage
	Answered bool
}

// Client is a minimal Managed Agents REST client — only what the
// pipeline uses, no SDK dependency (matching the repo's taste for tiny
// dependencies; the API surface here is six endpoints).
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com",
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// agentTools is the base toolset every template's agent gets: web
// research plus file read/write (oversized tool outputs land in sandbox
// files the agent must read back). No bash — the agents only write text
// (ADR 0009). The registry composes it with each template's submit tool.
var agentTools = []map[string]any{
	{
		"type":           "agent_toolset_20260401",
		"default_config": map[string]any{"enabled": false},
		"configs": []map[string]any{
			{"name": "web_search", "enabled": true},
			{"name": "web_fetch", "enabled": true},
			{"name": "read", "enabled": true},
			{"name": "write", "enabled": true},
		},
	},
}

type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("anthropic: HTTP %d: %s", e.status, e.body)
}

// do runs one JSON request. out may be nil.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "managed-agents-2026-04-01")
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return &apiError{status: resp.StatusCode, body: snippet}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// listPage is the common paginated list envelope.
type listPage[T any] struct {
	Data     []T    `json:"data"`
	NextPage string `json:"next_page"`
}

// findByName pages through a list endpoint until match returns true.
func findByName[T any](ctx context.Context, c *Client, path string, match func(T) bool) (*T, error) {
	page := ""
	for {
		q := "?limit=100"
		if page != "" {
			q += "&page=" + url.QueryEscape(page)
		}
		var pg listPage[T]
		if err := c.do(ctx, http.MethodGet, path+q, nil, &pg); err != nil {
			return nil, err
		}
		for i := range pg.Data {
			if match(pg.Data[i]) {
				return &pg.Data[i], nil
			}
		}
		if pg.NextPage == "" || pg.NextPage == "null" {
			return nil, nil
		}
		page = pg.NextPage
	}
}

type agentInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

func (c *Client) EnsureAgent(ctx context.Context, name, model, system string, tools []map[string]any) (string, error) {
	found, err := findByName(ctx, c, "/v1/agents", func(a agentInfo) bool { return a.Name == name })
	if err != nil {
		return "", err
	}
	if found == nil {
		var created agentInfo
		err := c.do(ctx, http.MethodPost, "/v1/agents", map[string]any{
			"name":   name,
			"model":  map[string]any{"id": model},
			"system": system,
			"tools":  tools,
		}, &created)
		if err != nil {
			return "", err
		}
		return created.ID, nil
	}
	// The update requires the current version (optimistic concurrency)
	// and has no-op detection: an unchanged configuration creates no new
	// version, so pushing the desired state on every boot is safe.
	update := func(version int) error {
		return c.do(ctx, http.MethodPost, "/v1/agents/"+found.ID, map[string]any{
			"version": version,
			"model":   map[string]any{"id": model},
			"system":  system,
			"tools":   tools,
		}, nil)
	}
	err = update(found.Version)
	if isConflict(err) {
		// Raced another updater; refetch the version and try once more.
		var a agentInfo
		if err := c.do(ctx, http.MethodGet, "/v1/agents/"+found.ID, nil, &a); err != nil {
			return "", err
		}
		err = update(a.Version)
	}
	if err != nil {
		return "", err
	}
	return found.ID, nil
}

func isConflict(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.status == http.StatusConflict
}

type environmentInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) EnsureEnvironment(ctx context.Context, name string) (string, error) {
	found, err := findByName(ctx, c, "/v1/environments", func(e environmentInfo) bool { return e.Name == name })
	if err != nil {
		return "", err
	}
	if found != nil {
		return found.ID, nil
	}
	var created environmentInfo
	// The sandbox only does web research through the built-in tools (no
	// bash), so networking mode barely matters; unrestricted is the
	// platform default.
	err = c.do(ctx, http.MethodPost, "/v1/environments", map[string]any{
		"name": name,
		"config": map[string]any{
			"type":       "cloud",
			"networking": map[string]any{"type": "unrestricted"},
		},
	}, &created)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *Client) CreateSession(ctx context.Context, agentID, envID, title string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          agentID,
		"environment_id": envID,
		"title":          title,
	}, &created)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *Client) SendMessage(ctx context.Context, sessionID, text string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{
		"events": []map[string]any{{
			"type":    "user.message",
			"content": []map[string]any{{"type": "text", "text": text}},
		}},
	}, nil)
}

func (c *Client) SessionStatus(ctx context.Context, sessionID string) (string, error) {
	var s struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+sessionID, nil, &s); err != nil {
		return "", err
	}
	return s.Status, nil
}

// Usage is a session's aggregate token consumption, flattened for the
// Generation meters. Careful: the session object is NOT the flat
// model_usage shape that span events use — cache writes arrive as a
// nested cache_creation object keyed by TTL, and a flat
// cache_creation_input_tokens field silently parses as 0.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

func (c *Client) SessionUsage(ctx context.Context, sessionID string) (Usage, error) {
	var s struct {
		Usage struct {
			InputTokens     int64 `json:"input_tokens"`
			OutputTokens    int64 `json:"output_tokens"`
			CacheReadTokens int64 `json:"cache_read_input_tokens"`
			CacheCreation   struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+sessionID, nil, &s); err != nil {
		return Usage{}, err
	}
	return Usage{
		InputTokens:      s.Usage.InputTokens,
		OutputTokens:     s.Usage.OutputTokens,
		CacheReadTokens:  s.Usage.CacheReadTokens,
		CacheWriteTokens: s.Usage.CacheCreation.Ephemeral5m + s.Usage.CacheCreation.Ephemeral1h,
	}, nil
}

type sessionEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// agent.custom_tool_use events
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// user.custom_tool_result events
	CustomToolUseID string `json:"custom_tool_use_id"`
}

func (c *Client) LastAgentMessage(ctx context.Context, sessionID string) (string, error) {
	last := ""
	page := ""
	for {
		q := "?limit=100"
		if page != "" {
			q += "&page=" + url.QueryEscape(page)
		}
		var pg listPage[sessionEvent]
		if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+sessionID+"/events"+q, nil, &pg); err != nil {
			return "", err
		}
		for _, ev := range pg.Data {
			if ev.Type != "agent.message" {
				continue
			}
			text := ""
			for _, block := range ev.Content {
				if block.Type == "text" {
					text += block.Text
				}
			}
			if text != "" {
				last = text
			}
		}
		if pg.NextPage == "" || pg.NextPage == "null" {
			return last, nil
		}
		page = pg.NextPage
	}
}

func (c *Client) LastToolUse(ctx context.Context, sessionID, name string) (*ToolUse, error) {
	var last *ToolUse
	answered := map[string]bool{}
	page := ""
	for {
		q := "?limit=100"
		if page != "" {
			q += "&page=" + url.QueryEscape(page)
		}
		var pg listPage[sessionEvent]
		if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+sessionID+"/events"+q, nil, &pg); err != nil {
			return nil, err
		}
		for _, ev := range pg.Data {
			switch ev.Type {
			case "agent.custom_tool_use":
				if ev.Name == name {
					last = &ToolUse{ID: ev.ID, Input: ev.Input}
				}
			case "user.custom_tool_result":
				answered[ev.CustomToolUseID] = true
			}
		}
		if pg.NextPage == "" || pg.NextPage == "null" {
			break
		}
		page = pg.NextPage
	}
	if last != nil {
		last.Answered = answered[last.ID]
	}
	return last, nil
}

func (c *Client) SendToolResult(ctx context.Context, sessionID, toolUseID, text string, isError bool) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{
		"events": []map[string]any{{
			"type":               "user.custom_tool_result",
			"custom_tool_use_id": toolUseID,
			"content":            []map[string]any{{"type": "text", "text": text}},
			"is_error":           isError,
		}},
	}, nil)
}

func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sessions/"+sessionID, nil, nil)
}

func (c *Client) CompleteJSON(ctx context.Context, model, prompt string, schema map[string]any, maxTokens int) (string, Usage, error) {
	var resp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens     int64 `json:"input_tokens"`
			OutputTokens    int64 `json:"output_tokens"`
			CacheReadTokens int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/messages", map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
		"output_config": map[string]any{
			"format": map[string]any{"type": "json_schema", "schema": schema},
		},
	}, &resp)
	if err != nil {
		return "", Usage{}, err
	}
	usage := Usage{
		InputTokens:     resp.Usage.InputTokens,
		OutputTokens:    resp.Usage.OutputTokens,
		CacheReadTokens: resp.Usage.CacheReadTokens,
	}
	switch resp.StopReason {
	case "refusal":
		return "", usage, fmt.Errorf("completion refused")
	case "max_tokens":
		return "", usage, fmt.Errorf("completion truncated at %d tokens", maxTokens)
	}
	text := ""
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	if strings.TrimSpace(text) == "" {
		return "", usage, fmt.Errorf("completion returned no text")
	}
	return text, usage, nil
}
