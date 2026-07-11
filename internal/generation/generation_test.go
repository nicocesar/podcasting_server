package generation

import (
	"strings"
	"testing"
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
	sc, err := ParseSubmission([]byte(`{"title":"T","summary":"S","language":"en","script":"Hello there.","sources":[{"title":"A","url":"https://a.example"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Title != "T" || sc.Language != "en" || len(sc.Sources) != 1 {
		t.Fatalf("parsed %+v", sc)
	}
	for _, bad := range []string{`{"title":"only"}`, `{"script":"only"}`, `[1,2]`} {
		if _, err := ParseSubmission([]byte(bad)); err == nil {
			t.Fatalf("ParseSubmission(%s) succeeded, want error", bad)
		}
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
	if !ValidFreshness(30) || ValidFreshness(2) {
		t.Fatal("ValidFreshness wrong")
	}
}
