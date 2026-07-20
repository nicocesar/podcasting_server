package generation

import (
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// The empty ID is every pre-template Generation: it must resolve to news
// and produce the exact task message the old pipeline sent.
func TestNewsTaskMessageUnchanged(t *testing.T) {
	tpl, ok := TemplateByID("")
	if !ok || tpl.ID != "news" {
		t.Fatalf("TemplateByID(\"\") = %+v, %v", tpl, ok)
	}
	g := store.Generation{Topic: "fusion", LengthMinutes: 5, FreshnessDays: 7, Language: "en"}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if got, want := tpl.TaskMessage(g, now), userMessage("fusion", 5, 7, "en", now); got != want {
		t.Errorf("news task message diverged from userMessage:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnknownTemplate(t *testing.T) {
	if _, ok := TemplateByID("nope"); ok {
		t.Error("unknown template resolved")
	}
}

func TestStoriesTaskMessage(t *testing.T) {
	tpl, ok := TemplateByID("stories")
	if !ok {
		t.Fatal("no stories template")
	}
	g := store.Generation{
		Topic:         "a dragon afraid of heights",
		LengthMinutes: 2,
		AgeRange:      "5-7",
		Language:      "es",
		Cast: []store.Character{
			{Name: "Lila", Description: "A brave young fox."},
			{Name: "Grandpa Bear", Description: "Slow, warm, always hungry."},
		},
	}
	msg := tpl.TaskMessage(g, time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC))
	for _, want := range []string{
		"a dragon afraid of heights",
		"aged 5 to 7",
		"300 spoken words", // 2 min × 150 wpm
		"Spanish",
		"Returning characters",
		"Lila — A brave young fox.",
		"Grandpa Bear — Slow, warm, always hungry.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("task missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Freshness") {
		t.Errorf("stories task mentions freshness:\n%s", msg)
	}

	// Without a cast, the returning-characters block is absent.
	g.Cast = nil
	if msg := tpl.TaskMessage(g, time.Now()); strings.Contains(msg, "Returning characters") {
		t.Errorf("cast block without a cast:\n%s", msg)
	}
}

func TestValidAgeRange(t *testing.T) {
	for _, o := range AgeRanges {
		if !ValidAgeRange(o.Value) {
			t.Errorf("offered band %q not valid", o.Value)
		}
	}
	for _, bad := range []string{"", "0-99", "adult"} {
		if ValidAgeRange(bad) {
			t.Errorf("band %q accepted", bad)
		}
	}
}
