package generation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/music"
)

// compInput builds a submit_music payload with the given movement
// durations, all prompts non-empty.
func compInput(durations ...int) []byte {
	movs := make([]map[string]any, len(durations))
	for i, d := range durations {
		movs[i] = map[string]any{"prompt": fmt.Sprintf("movement %d, warm rhodes, 60bpm", i+1), "duration_ms": d}
	}
	b, _ := json.Marshal(map[string]any{
		"title": "The Long Room", "summary": "Slow piano for a rainy evening.", "movements": movs,
	})
	return b
}

func TestParseMusicSubmissionAccepts(t *testing.T) {
	// 25 minutes as three movements, none over the ten-minute ceiling.
	c, err := ParseMusicSubmission(compInput(600_000, 600_000, 300_000), 25)
	if err != nil {
		t.Fatalf("ParseMusicSubmission: %v", err)
	}
	if len(c.Movements) != 3 {
		t.Fatalf("movements = %d, want 3", len(c.Movements))
	}
	if c.Title != "The Long Room" {
		t.Errorf("title = %q", c.Title)
	}
	if got, want := c.TotalMS(), 1_500_000; got != want {
		t.Errorf("TotalMS = %d, want %d", got, want)
	}
	if c.Description() != "Slow piano for a rainy evening." {
		t.Errorf("Description = %q", c.Description())
	}
}

// TestParseMusicSubmissionDurationBounds is the one that matters most:
// the vendor caps a single generation at ten minutes, so a movement over
// it must be rejected here rather than failing mid-chain after earlier
// movements have already been paid for.
func TestParseMusicSubmissionDurationBounds(t *testing.T) {
	if _, err := ParseMusicSubmission(compInput(music.MaxDurationMS), 10); err != nil {
		t.Errorf("exactly the ceiling should be accepted: %v", err)
	}
	if _, err := ParseMusicSubmission(compInput(music.MaxDurationMS+1), 10); err == nil {
		t.Error("a movement over the ceiling should be rejected")
	}
	if _, err := ParseMusicSubmission(compInput(music.MinDurationMS-1), 0); err == nil {
		t.Error("a movement under the floor should be rejected")
	}
}

func TestParseMusicSubmissionRejects(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
		mins  int
		want  string // substring the agent needs to see
	}{
		{"no movements", []byte(`{"title":"T","summary":"S","movements":[]}`), 5, "no movements"},
		{"missing title", []byte(`{"summary":"S","movements":[{"prompt":"p","duration_ms":300000}]}`), 5, "title"},
		{"missing summary", []byte(`{"title":"T","movements":[{"prompt":"p","duration_ms":300000}]}`), 5, "summary"},
		{"empty prompt", []byte(`{"title":"T","summary":"S","movements":[{"prompt":"  ","duration_ms":300000}]}`), 5, "empty prompt"},
		{"not json", []byte(`{nope`), 5, "contract"},
		// Asked for 25 minutes, planned 10: the piece would end early.
		{"total too short", compInput(600_000), 25, "add up"},
		{"total too long", compInput(600_000, 600_000, 600_000), 5, "add up"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMusicSubmission(tc.input, tc.mins)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// TestParseMusicSubmissionTolerance: the agent splits a target by hand,
// so being a little off is not worth a rejection round-trip.
func TestParseMusicSubmissionTolerance(t *testing.T) {
	// 10 minutes requested, 9m30s planned — 5% under.
	if _, err := ParseMusicSubmission(compInput(570_000), 10); err != nil {
		t.Errorf("within tolerance should be accepted: %v", err)
	}
	// 10 minutes requested, 8 minutes planned — 20% under.
	if _, err := ParseMusicSubmission(compInput(480_000), 10); err == nil {
		t.Error("outside tolerance should be rejected")
	}
}

// TestTemplateSubmitTools guards the split introduced with the ambient
// program: the spoken templates must keep answering on submit_episode.
func TestTemplateSubmitTools(t *testing.T) {
	for _, id := range []string{"news", "stories"} {
		tpl, ok := TemplateByID(id)
		if !ok {
			t.Fatalf("template %q missing", id)
		}
		if tpl.SubmitToolName != submitToolName {
			t.Errorf("%s submits on %q, want %q", id, tpl.SubmitToolName, submitToolName)
		}
		if tpl.IsMusic {
			t.Errorf("%s should not be a music template", id)
		}
	}
	tpl, ok := TemplateByID("ambient")
	if !ok {
		t.Fatal("ambient template missing")
	}
	if tpl.SubmitToolName != submitMusicToolName {
		t.Errorf("ambient submits on %q, want %q", tpl.SubmitToolName, submitMusicToolName)
	}
	if !tpl.IsMusic {
		t.Error("ambient should be a music template")
	}
	// No web tools: there is nothing to research, and a composer that
	// starts searching the web is burning tokens for nothing.
	if len(tpl.Tools) != 1 {
		t.Errorf("ambient should carry only its submit tool, got %d tools", len(tpl.Tools))
	}
	// An empty template id still means news (every Generation predating
	// the Template field was a briefing).
	if def, _ := TemplateByID(""); def.ID != "news" {
		t.Errorf(`TemplateByID("") = %q, want news`, def.ID)
	}
}
