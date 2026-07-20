package generation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The session object nests cache writes under cache_creation by TTL —
// this is a verbatim (trimmed) production response. A flat
// cache_creation_input_tokens field parses as 0 and silently zeroes the
// cache-write meter, which is how episodes came out priced at a third
// of reality.
func TestSessionUsageParsesNestedCacheCreation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/sesn_test" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"type":"session","id":"sesn_test","usage":{
			"cache_creation":{"ephemeral_1h_input_tokens":100,"ephemeral_5m_input_tokens":49732},
			"cache_read_input_tokens":111238,
			"input_tokens":10,
			"output_tokens":3278}}`)
	}))
	defer ts.Close()

	c := NewClient("sk-ant-test")
	c.baseURL = ts.URL
	u, err := c.SessionUsage(context.Background(), "sesn_test")
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{InputTokens: 10, OutputTokens: 3278, CacheReadTokens: 111238, CacheWriteTokens: 49832}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestCompleteJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model        string `json:"model"`
			OutputConfig struct {
				Format struct {
					Type   string         `json:"type"`
					Schema map[string]any `json:"schema"`
				} `json:"format"`
			} `json:"output_config"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "claude-test" || req.OutputConfig.Format.Type != "json_schema" || req.OutputConfig.Format.Schema == nil {
			t.Errorf("request = %s", body)
		}
		fmt.Fprint(w, `{"stop_reason":"end_turn",
			"content":[{"type":"text","text":"{\"characters\":[]}"}],
			"usage":{"input_tokens":12,"output_tokens":7}}`)
	}))
	defer ts.Close()

	c := NewClient("sk-ant-test")
	c.baseURL = ts.URL
	out, u, err := c.CompleteJSON(context.Background(), "claude-test", "hi", map[string]any{"type": "object"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"characters":[]}` {
		t.Errorf("out = %q", out)
	}
	if u.InputTokens != 12 || u.OutputTokens != 7 {
		t.Errorf("usage = %+v", u)
	}
}

func TestCompleteJSONTruncated(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"stop_reason":"max_tokens","content":[{"type":"text","text":"{"}],"usage":{}}`)
	}))
	defer ts.Close()

	c := NewClient("sk-ant-test")
	c.baseURL = ts.URL
	_, _, err := c.CompleteJSON(context.Background(), "claude-test", "hi", map[string]any{}, 5)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("err = %v, want truncation error", err)
	}
}

func TestExtractCharacters(t *testing.T) {
	api := newFakeAPI()
	api.completions = []string{`{"characters":[
		{"name":"Lila","description":"A brave young fox."},
		{"name":"Grandpa Bear","description":"Slow and warm."}]}`}
	chars, u, err := ExtractCharacters(context.Background(), api, "Once upon a time…")
	if err != nil {
		t.Fatal(err)
	}
	if len(chars) != 2 || chars[0].Name != "Lila" || chars[1].Description != "Slow and warm." {
		t.Errorf("characters = %+v", chars)
	}
	if u.InputTokens == 0 {
		t.Error("usage not reported")
	}

	// An empty cast is an error: there is nothing to save.
	api.completions = []string{`{"characters":[]}`}
	if _, _, err := ExtractCharacters(context.Background(), api, "…"); err == nil {
		t.Error("empty cast accepted")
	}
}
