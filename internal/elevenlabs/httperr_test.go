package elevenlabs

import (
	"errors"
	"strings"
	"testing"
)

// concurrentLimitBody is the real payload behind the failure that
// prompted this package: a Generation died on "music: http 429: {...}"
// and the reader could not tell ElevenLabs from the agent API, both of
// which rate-limit with a 429.
const concurrentLimitBody = `{"detail":{"type":"rate_limit_error","code":"concurrent_limit_exceeded",` +
	`"message":"Too many concurrent requests. Your current subscription is associated with a maximum of 2 ` +
	`concurrent requests (running in parallel). This is done such that a single user does not overwhelm our ` +
	`systems and affect other users negatively. Please upgrade your subscription or contact sales if you want ` +
	`to increase this limit.","status":"too_many_concurrent_requests",` +
	`"request_id":"ad65dae6c394803c203c840a541b4fd3","docs_url":"https://elevenlabs.io/docs"}}`

func TestConcurrentLimitNamesTheVendorAndTheAction(t *testing.T) {
	err := HTTPError("music", 429, []byte(concurrentLimitBody))
	got := err.Error()

	// Who refused, on which product, and what to do about it — the
	// three things the old message left the reader to infer.
	for _, want := range []string{
		"elevenlabs music",
		"http 429",
		"concurrent_limit_exceeded",
		"Too many concurrent requests.",
		"wait for it to finish and retry",
		"request ad65dae6c394803c203c840a541b4fd3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error missing %q:\n%s", want, got)
		}
	}

	// The vendor's policy paragraph and sales pitch are not the user's
	// problem; the hint already carries the action.
	for _, unwanted := range []string{"contact sales", "overwhelm our systems", "docs_url"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("error still carries boilerplate %q:\n%s", unwanted, got)
		}
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T", err)
	}
	if !apiErr.RateLimited() {
		t.Error("429 should report RateLimited")
	}
	if apiErr.Code != "concurrent_limit_exceeded" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestSurfaceDistinguishesMusicFromSpeech(t *testing.T) {
	music := HTTPError("music", 429, []byte(concurrentLimitBody)).Error()
	speech := HTTPError("text-to-speech", 429, []byte(concurrentLimitBody)).Error()
	if !strings.Contains(music, "elevenlabs music") {
		t.Errorf("music surface: %s", music)
	}
	if !strings.Contains(speech, "elevenlabs text-to-speech") {
		t.Errorf("speech surface: %s", speech)
	}
}

func TestHints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"paid plan", 402, `{"detail":{"code":"paid_plan_required","message":"Upgrade."}}`,
			"ElevenLabs credits are exhausted"},
		// The failure that started this: an empty balance answers 401
		// with a "status", not a "code", and no mention of a key. The
		// old message sent the operator hunting a bad secret.
		{"out of credit", 401,
			`{"detail":{"status":"quota_exceeded","message":"This request exceeds your quota of 100000. You have 0 credits remaining, while 91 credits are required for this request."}}`,
			"ElevenLabs credits are exhausted"},
		{"bad key", 401, `{"detail":{"message":"Invalid API key."}}`, "check ELEVENLABS_API_KEY"},
		{"plain rate limit", 429, `{"detail":{"message":"Slow down."}}`, "rate limited; retry shortly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := HTTPError("music", tc.status, []byte(tc.body)).Error()
			if !strings.Contains(got, tc.want) {
				t.Errorf("missing hint %q:\n%s", tc.want, got)
			}
		})
	}

	// A 500 has no unambiguous action, so it offers none rather than
	// inventing one about the operator's account.
	got := HTTPError("music", 500, []byte(`{"detail":{"message":"Internal error."}}`)).Error()
	if strings.Contains(got, "—") {
		t.Errorf("500 invented a hint:\n%s", got)
	}
}

func TestUnparseableBodyStillNamesTheVendor(t *testing.T) {
	// A proxy's HTML error page, or an empty body: the vendor and the
	// status are known regardless, and that is most of the answer.
	for _, body := range []string{"<html>502 Bad Gateway</html>", ""} {
		got := HTTPError("music", 502, []byte(body)).Error()
		if !strings.Contains(got, "elevenlabs music: http 502") {
			t.Errorf("body %q lost the vendor:\n%s", body, got)
		}
	}
}

func TestDetailAsBareString(t *testing.T) {
	got := HTTPError("music", 422, []byte(`{"detail":"music_length_ms must be <= 600000"}`)).Error()
	if !strings.Contains(got, "music_length_ms must be <= 600000") {
		t.Errorf("bare-string detail lost:\n%s", got)
	}
}

func TestLongMessageIsCapped(t *testing.T) {
	long := strings.Repeat("word ", 200) // no sentence breaks to cut at
	got := HTTPError("music", 400, []byte(`{"detail":{"message":"`+long+`"}}`)).Error()
	if len(got) > 400 {
		t.Errorf("error is %d chars, want a one-liner:\n%s", len(got), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("truncation not marked:\n%s", got)
	}
}

// TestQuotaIsNotABadKey guards the distinction that misled us: both are
// 401s, and only one is fixed by touching the key.
func TestQuotaIsNotABadKey(t *testing.T) {
	quota := HTTPError("music", 401,
		[]byte(`{"detail":{"status":"quota_exceeded","message":"This request exceeds your quota of 100000."}}`))
	badKey := HTTPError("music", 401, []byte(`{"detail":{"status":"invalid_api_key","message":"Invalid API key."}}`))

	var q, b *APIError
	if !errors.As(quota, &q) || !errors.As(badKey, &b) {
		t.Fatal("expected *APIError from both")
	}
	if !q.Quota() {
		t.Error("quota_exceeded should report Quota()")
	}
	if b.Quota() {
		t.Error("a rejected key is not a quota problem")
	}
	if strings.Contains(quota.Error(), "ELEVENLABS_API_KEY") {
		t.Errorf("out-of-credit still blames the key:\n%s", quota.Error())
	}
	if !strings.Contains(badKey.Error(), "ELEVENLABS_API_KEY") {
		t.Errorf("bad key should still point at the key:\n%s", badKey.Error())
	}
}

// TestMissingPermissionIsNotABadKey is the failure that met the first
// real call to /v1/user/subscription: a valid key without the user_read
// scope. Telling the operator to "check ELEVENLABS_API_KEY" would send
// them hunting a typo in a key that works fine everywhere else.
func TestMissingPermissionIsNotABadKey(t *testing.T) {
	body := `{"detail":{"type":"authentication_error","code":"unauthorized",` +
		`"message":"The API key you used is missing the permission user_read to execute this operation.",` +
		`"status":"missing_permissions","request_id":"e33272383f89e4085e0dc41998089d83"}}`
	got := HTTPError("subscription", 401, []byte(body)).Error()

	if !strings.Contains(got, "user_read") {
		t.Errorf("does not name the missing permission:\n%s", got)
	}
	if strings.Contains(got, "check ELEVENLABS_API_KEY") {
		t.Errorf("blames the key for a permissions problem:\n%s", got)
	}
	var e *APIError
	if errors.As(HTTPError("subscription", 401, []byte(body)), &e) && e.Quota() {
		t.Error("a permissions error is not an exhausted balance")
	}
}
