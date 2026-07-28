package elevenlabs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// LowRemainingFraction is where "plenty left" becomes "top this up": a
// tenth of the allowance. Below it the admin surfaces start saying so,
// because the failure it precedes is not graceful — music stops being
// possible at all, and only speech has somewhere else to go.
const LowRemainingFraction = 0.10

// Subscription is the balance behind the key: what ElevenLabs bills
// against and what runs out. Credits are their unit — the API counts
// characters, and the same pool pays for both speech and music — so
// this deliberately reports no dollars. Turning credits into currency
// would mean a price table, which this project has refused once already
// for Anthropic tokens (ADR 0024's reasoning, same conclusion).
type Subscription struct {
	Tier   string `json:"tier"`
	Status string `json:"status"`
	// Used and Limit are ElevenLabs' character_count and
	// character_limit for the current billing period.
	Used  int64 `json:"character_count"`
	Limit int64 `json:"character_limit"`
	// ResetUnix is when the allowance refills, 0 if they did not say.
	ResetUnix int64 `json:"next_character_count_reset_unix"`
}

// Remaining is the credit left in this period, never negative.
func (s Subscription) Remaining() int64 {
	if n := s.Limit - s.Used; n > 0 {
		return n
	}
	return 0
}

// RemainingFraction is what is left as a share of the allowance. A zero
// limit reports 0 rather than dividing by it.
func (s Subscription) RemainingFraction() float64 {
	if s.Limit <= 0 {
		return 0
	}
	return float64(s.Remaining()) / float64(s.Limit)
}

// UsedPercent is the share consumed, for a progress bar.
func (s Subscription) UsedPercent() int {
	if s.Limit <= 0 {
		return 100
	}
	p := int(float64(s.Used) / float64(s.Limit) * 100)
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	}
	return p
}

// Exhausted reports nothing left to spend: music is impossible and
// speech is on its fallback engines.
func (s Subscription) Exhausted() bool { return s.Remaining() == 0 }

// Low reports a balance worth warning an admin about, exhausted
// included. A dead or suspended subscription counts however much credit
// it nominally shows.
func (s Subscription) Low() bool {
	switch s.Status {
	case "free_disabled", "past_due", "incomplete":
		return true
	}
	return s.RemainingFraction() < LowRemainingFraction
}

// Resets is when the allowance refills, zero if unknown.
func (s Subscription) Resets() time.Time {
	if s.ResetUnix == 0 {
		return time.Time{}
	}
	return time.Unix(s.ResetUnix, 0).UTC()
}

// Client reads account-level facts that no generation call reports:
// chiefly how much credit is left before the next 401.
type Client struct {
	key     string
	baseURL string
	http    *http.Client
}

// NewClient returns nil for an empty key, so a deployment without
// ElevenLabs simply has no balance to show rather than an error to
// handle on every admin page.
func NewClient(key string) *Client { return NewClientAt(key, "") }

// NewClientAt points the client at another host (tests only). An empty
// baseURL means the real API.
func NewClientAt(key, baseURL string) *Client {
	if key == "" {
		return nil
	}
	if baseURL == "" {
		baseURL = "https://api.elevenlabs.io"
	}
	return &Client{
		key:     key,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Subscription fetches the current balance.
func (c *Client) Subscription(ctx context.Context) (Subscription, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/user/subscription", nil)
	if err != nil {
		return Subscription{}, err
	}
	req.Header.Set("xi-api-key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return Subscription{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Subscription{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Subscription{}, HTTPError("subscription", resp.StatusCode, body)
	}
	var s Subscription
	if err := json.Unmarshal(body, &s); err != nil {
		return Subscription{}, err
	}
	return s, nil
}
