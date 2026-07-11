package generation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
