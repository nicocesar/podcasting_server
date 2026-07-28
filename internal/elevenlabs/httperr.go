// Package elevenlabs holds what the music and speech clients share:
// how a refusal from ElevenLabs is turned into an error someone can act
// on without knowing whose JSON envelope they are reading.
package elevenlabs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// APIError is a non-2xx response from ElevenLabs. It exists so a failed
// Generation can say which vendor refused it: "music: http 429" left a
// reader guessing between the composer, the voice engine and the agent
// API, all three of which can rate-limit a single episode.
type APIError struct {
	// Surface is the ElevenLabs product involved — "music" or
	// "text-to-speech" — not our package name.
	Surface string
	Status  int
	// Code and Message are ElevenLabs' own, when the body parses.
	Code    string
	Message string
	// Reason is their "status" string, which is often the specific one
	// while Code stays generic: a key without a scope is code
	// "unauthorized", reason "missing_permissions".
	Reason string
	// RequestID is what their support asks for first.
	RequestID string
	// Raw is the (capped) body, kept for the unparseable case.
	Raw string
}

// RateLimited reports the one failure worth retrying unchanged: the
// request was fine, there were merely too many of them.
func (e *APIError) RateLimited() bool { return e.Status == 429 }

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "elevenlabs %s: http %d", e.Surface, e.Status)
	if e.Code != "" {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	if msg := e.Message; msg != "" {
		fmt.Fprintf(&b, ": %s", msg)
	} else if e.Raw != "" {
		fmt.Fprintf(&b, ": %s", e.Raw)
	}
	if h := e.hint(); h != "" {
		fmt.Fprintf(&b, " — %s", h)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request %s)", e.RequestID)
	}
	return b.String()
}

// hint is the sentence that turns a status code into an action. Only
// for failures where the action is unambiguous; anything else says
// nothing rather than guessing at the operator's account.
func (e *APIError) hint() string {
	switch {
	// Out of credit answers 401, the same status a bad key does. It is
	// the likeliest failure on a busy month and the one whose fix is
	// nothing to do with the key, so it is tested first.
	case e.Quota():
		return "ElevenLabs credits are exhausted; top up the plan (music stops entirely, speech falls back to another engine)"
	// A scoped key: the secret is valid, it just may not do this. Worth
	// its own sentence, since "check ELEVENLABS_API_KEY" would send the
	// operator looking for a typo in a key that is perfectly good.
	case e.Reason == "missing_permissions":
		return "this API key lacks a permission the call needs (user_read reads the credit balance); grant it where the key was issued"
	case e.Code == "concurrent_limit_exceeded":
		return "another generation is already running on this key; wait for it to finish and retry"
	case e.Status == 429:
		return "rate limited; retry shortly"
	case e.Status == 401 || e.Status == 403:
		return "check ELEVENLABS_API_KEY"
	}
	return ""
}

// Quota reports an exhausted balance rather than a rejected key. The
// two are both 401s and read alike; only this tells the operator to add
// credit instead of hunting a bad secret.
func (e *APIError) Quota() bool {
	switch e.Code {
	case "quota_exceeded", "paid_plan_required":
		return true
	}
	switch e.Reason {
	case "quota_exceeded", "paid_plan_required":
		return true
	}
	return e.Status == 402 || strings.Contains(strings.ToLower(e.Message), "exceeds your quota")
}

// errorBody covers the shapes ElevenLabs answers with. Their API is
// FastAPI underneath, so the payload hangs off "detail" — either an
// object or, on validation errors, a bare string. The flat "message"
// form shows up on a few endpoints.
type errorBody struct {
	Detail  json.RawMessage `json:"detail"`
	Message string          `json:"message"`
}

type errorDetail struct {
	Type string `json:"type"`
	Code string `json:"code"`
	// Status is where the important ones actually live: an exhausted
	// balance is 401 with status "quota_exceeded" and no code at all.
	// Reading only "code" made that indistinguishable from a bad key.
	Status    string `json:"status"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// HTTPError builds the error for a failed response. surface names the
// ElevenLabs product ("music", "text-to-speech"); body is the response
// body, already capped by the caller. An unparseable body still yields
// a usable error — the vendor and the status are known regardless.
func HTTPError(surface string, status int, body []byte) error {
	e := &APIError{
		Surface: surface,
		Status:  status,
		Raw:     truncate(strings.TrimSpace(string(body)), 300),
	}
	var eb errorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		return e
	}
	e.Message = eb.Message
	if len(eb.Detail) > 0 {
		var d errorDetail
		if err := json.Unmarshal(eb.Detail, &d); err == nil {
			e.Code, e.Reason, e.RequestID = d.Code, d.Status, d.RequestID
			if d.Message != "" {
				e.Message = d.Message
			}
			// Fall back through the other two names the same idea goes
			// by, so "quota_exceeded" is never lost to an empty Code.
			for _, alt := range []string{d.Status, d.Type} {
				if e.Code == "" {
					e.Code = alt
				}
			}
		} else {
			// "detail" as a bare string: the whole message.
			var s string
			if err := json.Unmarshal(eb.Detail, &s); err == nil {
				e.Message = s
			}
		}
	}
	// Their rate-limit copy runs to a paragraph of policy and sales
	// pitch. The first sentences carry the fact; our hint carries the
	// action, so the rest is noise in a one-line error.
	e.Message = truncate(firstSentences(e.Message, 2), 200)
	return e
}

// firstSentences keeps at most n sentences of s, provided cutting there
// actually shortens it.
func firstSentences(s string, n int) string {
	count, cut := 0, -1
	for i, r := range s {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		count++
		if count == n {
			cut = i + 1
			break
		}
	}
	if cut <= 0 || cut >= len(s) {
		return s
	}
	return strings.TrimSpace(s[:cut])
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}
