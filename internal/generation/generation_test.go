package generation

import (
	"strings"
	"testing"
	"time"
)

func TestParseScript(t *testing.T) {
	payload := `{"title":"T","summary":"S","script":"Hello there.","sources":[{"title":"A","url":"https://a.example","published":"2026-07-01"}]}`
	cases := []struct {
		name string
		msg  string
	}{
		{"fenced", "Here is the episode.\n```json\n" + payload + "\n```\n"},
		{"bare", payload},
		{"fence after earlier fence", "```json\n{\"title\":\"draft\"}\n```\ntake two:\n```json\n" + payload + "\n```"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := ParseScript(tc.msg)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Title != "T" || sc.Script != "Hello there." || len(sc.Sources) != 1 {
				t.Fatalf("parsed %+v", sc)
			}
		})
	}

	for _, bad := range []string{"", "no json here", "```json\n{\"title\":\"only\"}\n```"} {
		if _, err := ParseScript(bad); err == nil {
			t.Fatalf("ParseScript(%q) succeeded, want error", bad)
		}
	}
}

func TestParseSubmission(t *testing.T) {
	sc, err := ParseSubmission([]byte(`{"title":"T","summary":"S","language":"en","script":"Hello there.","sources":[{"title":"A","url":"https://a.example"}]}`), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Title != "T" || sc.Language != "en" || len(sc.Sources) != 1 {
		t.Fatalf("parsed %+v", sc)
	}
	for _, bad := range []string{`{"title":"only"}`, `{"script":"only"}`, `[1,2]`} {
		if _, err := ParseSubmission([]byte(bad), 0, true); err == nil {
			t.Fatalf("ParseSubmission(%s) succeeded, want error", bad)
		}
	}
}

// A submission with nothing in it must not reach the checkpoint: once
// acceptScript stores it, Retry resumes from the stored Script and the
// episode is unrecoverable without a whole new run. Both cases below are
// taken from the andon-fm session, where the agent submitted a
// placeholder, was told it was accepted, and only then noticed.
func TestParseSubmissionRejectsEmptySubmission(t *testing.T) {
	sourced := `,"sources":[{"title":"A","url":"https://a.example"}]}`
	for _, tc := range []struct {
		name    string
		input   string
		minutes int
		want    string
	}{
		{
			name:    "placeholder script",
			input:   `{"title":"T","summary":"S","language":"en","script":"PLACEHOLDER"` + sourced,
			minutes: 5,
			want:    "1 words",
		},
		{
			name:    "no sources",
			input:   `{"title":"T","summary":"S","language":"en","script":"` + strings.Repeat("word ", 800) + `","sources":[]}`,
			minutes: 5,
			want:    "no sources",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSubmission([]byte(tc.input), tc.minutes, true)
			if err == nil {
				t.Fatal("submission accepted, want rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Story Time invents its tales, and its own prompt tells it to submit an
// empty sources list when no research was involved. A blanket sources
// requirement would reject every such story, and since the agent would
// keep resubmitting what it was told to, the run could only end at the
// session timeout. The requirement is the news template's, not a rule.
func TestParseSubmissionAllowsSourcelessFiction(t *testing.T) {
	input := `{"title":"T","summary":"S","language":"en","script":"` +
		strings.Repeat("word ", 800) + `","sources":[]}`
	if _, err := ParseSubmission([]byte(input), 5, false); err != nil {
		t.Fatalf("sourceless story rejected: %v", err)
	}
	// The same submission is still rejected where sources are the point.
	if _, err := ParseSubmission([]byte(input), 5, true); err == nil {
		t.Fatal("sourceless research episode accepted")
	}
}

// Short of the target but recognisably the episode: the length check is
// a substance floor, not the ten-percent style rule the prompt asks for,
// so a merely undersized script still lands.
func TestParseSubmissionAcceptsUndersizedScript(t *testing.T) {
	// 600 words against a 750-word (5-minute) budget.
	input := `{"title":"T","summary":"S","language":"en","script":"` +
		strings.Repeat("word ", 600) + `","sources":[{"title":"A","url":"https://a.example"}]}`
	if _, err := ParseSubmission([]byte(input), 5, true); err != nil {
		t.Fatalf("600 words against a 750-word budget rejected: %v", err)
	}
}

func TestScriptDescription(t *testing.T) {
	sc := Script{
		Summary: "What happened.",
		Sources: []Source{
			{Title: "A report", URL: "https://a.example", Published: "2026-07-01"},
			{Title: "No date", URL: "https://b.example"},
		},
	}
	d := sc.Description()
	for _, want := range []string{"What happened.", "Sources:", "A report (2026-07-01) — https://a.example", "No date — https://b.example"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description missing %q:\n%s", want, d)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fusion Energy!":                     "fusion-energy",
		"  ¿Qué pasa, mundo? ":               "qu-pasa-mundo",
		"":                                   "episode",
		"---":                                "episode",
		strings.Repeat("verylongtopic ", 10): "verylongtopic-verylongtopic-verylongtopi",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
		if len(Slugify(in)) > 41 {
			t.Errorf("Slugify(%q) too long", in)
		}
	}
}

func TestValidators(t *testing.T) {
	if !ValidLength(5) || ValidLength(7) {
		t.Fatal("ValidLength wrong")
	}
	if !ValidFreshness(30) || !ValidFreshness(0) || ValidFreshness(2) {
		t.Fatal("ValidFreshness wrong")
	}
	if !ValidInterval(7) || ValidInterval(0) || ValidInterval(2) {
		t.Fatal("ValidInterval wrong")
	}
	// The cadences are a subset of the Freshness Windows: one vocabulary
	// of durations, so "every week" and "last week" mean the same span.
	for _, o := range IntervalOptions {
		if !ValidFreshness(o.Days) {
			t.Errorf("interval %d days is not also a freshness window", o.Days)
		}
	}
}

// TestUserMessageStretchedWindow: a catching-up Beat asks for a window
// that is not on the form's menu — the gap since its last Episode. The
// task message has to render that number as it would any other, since
// dropping back to a menu value would silently lose the days the Beat
// was quiet for.
func TestUserMessageStretchedWindow(t *testing.T) {
	msg := userMessage("volcanoes", 5, 10, "en", time.Now())
	if !strings.Contains(msg, "the last 10 days") {
		t.Fatalf("stretched window not rendered: %q", msg)
	}
}

func TestUserMessageFreshness(t *testing.T) {
	dated := userMessage("volcanoes", 5, 7, "en", time.Now())
	if !strings.Contains(dated, "the last 7 days") {
		t.Fatalf("dated task missing window: %q", dated)
	}
	timeless := userMessage("volcanoes", 5, 0, "en", time.Now())
	if !strings.Contains(timeless, "timeless") || strings.Contains(timeless, "last 0 days") {
		t.Fatalf("timeless task wrong: %q", timeless)
	}
}
