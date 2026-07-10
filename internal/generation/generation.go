// Package generation runs the built-in Generation pipeline (ADR 0009):
// a managed-agent session researches the Topic inside its Freshness
// Window and writes the Script; the server voices it (internal/tts) and
// publishes the Episode into the requester's own Personal Feed. Every
// step checkpoints into the store.Generation record so any instance can
// resume after a restart.
package generation

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Lengths are the Target Length options (minutes), as offered on the
// form. The word budget assumes ~150 spoken words per minute.
var Lengths = []int{2, 5, 10, 15, 25, 60}

const wordsPerMinute = 150

// FreshnessOption is one Freshness Window choice.
type FreshnessOption struct {
	Days  int
	Label string
}

var FreshnessOptions = []FreshnessOption{
	{1, "Last 24 hours"},
	{3, "Last 3 days"},
	{7, "Last week"},
	{14, "Last 2 weeks"},
	{30, "Last month"},
	{90, "Last 3 months"},
	{365, "Last year"},
}

// ValidLength reports whether minutes is one of the offered options.
func ValidLength(minutes int) bool { return slices.Contains(Lengths, minutes) }

// ValidFreshness reports whether days is one of the offered windows.
func ValidFreshness(days int) bool {
	for _, o := range FreshnessOptions {
		if o.Days == days {
			return true
		}
	}
	return false
}

// Script is the agent's output: the durable midpoint of a Generation.
// Once stored, a later failure never repeats the research.
type Script struct {
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	Language string   `json:"language,omitempty"` // agent-reported BCP-47 tag of the script text
	Script   string   `json:"script"`             // spoken text, plain prose
	Sources  []Source `json:"sources"`
}

// PrimaryTag normalizes a BCP-47 tag to its primary subtag ("es-ES" →
// "es"), the granularity the Language options use. Empty in, empty out.
func PrimaryTag(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	return lang
}

type Source struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Published string `json:"published,omitempty"`
}

// ParseScript extracts the Script from the agent's final message: the
// last ```json fence, or the whole message if it is bare JSON.
func ParseScript(msg string) (Script, error) {
	raw := strings.TrimSpace(msg)
	if i := strings.LastIndex(raw, "```json"); i >= 0 {
		raw = raw[i+len("```json"):]
		if j := strings.Index(raw, "```"); j >= 0 {
			raw = raw[:j]
		}
	}
	var sc Script
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &sc); err != nil {
		return Script{}, fmt.Errorf("agent output is not the agreed JSON: %w", err)
	}
	if sc.Title == "" || sc.Script == "" {
		return Script{}, fmt.Errorf("agent output is missing title or script")
	}
	return sc, nil
}

// Description renders the Episode description: the summary plus a dated
// sources list, making the Freshness Window auditable from the feed.
func (sc Script) Description() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(sc.Summary))
	if len(sc.Sources) > 0 {
		b.WriteString("\n\nSources:")
		for _, src := range sc.Sources {
			b.WriteString("\n- " + strings.TrimSpace(src.Title))
			if src.Published != "" {
				b.WriteString(" (" + src.Published + ")")
			}
			if src.URL != "" {
				b.WriteString(" — " + src.URL)
			}
		}
	}
	return b.String()
}

// systemPrompt is the pre-baked agent's behavior. It lives in the repo
// on purpose: the startup bootstrap pushes it to the platform, where a
// change becomes a new agent version (ADR 0009).
const systemPrompt = `You are the episode writer for a private podcast service that produces news-like audio briefings. Each task message gives you a topic, a target spoken length in words, a freshness window, a language, and today's date. Your job: research the topic on the web, then write a complete, ready-to-voice podcast episode script.

Research rules:
- Use web search (and web fetch on promising results) to find out what has happened around the topic.
- Anchor the episode in developments from sources published within the freshness window. Older material may be used for background and context only.
- If the window contains little or nothing new on the topic, say so naturally in the episode itself, and cover the freshest material available instead. Never refuse the task for lack of news.
- Prefer primary and reputable sources; note each source's publication date.

Writing rules:
- Write in the requested language, and only that language — even when most or all of your sources are in a different one. Research in whatever language the sources use; the episode itself is always in the requested language.
- The script is read aloud by a single narrator: plain flowing prose. No markdown, no headings, no bullet points, no URLs, no stage directions, nothing a voice cannot speak.
- Mention dates and recency naturally ("this Tuesday", "earlier this month") so listeners can place events in time.
- Open by saying what the episode covers; close with a brief sign-off.
- Hit the target word count within about ten percent. Do not pad with filler; if the well runs dry, go deeper on fewer stories.

Output contract — this is parsed by a machine:
When the episode is ready, reply with one final message whose entire content is a single fenced code block, opened with three backticks and the word json, containing exactly one JSON object with these fields and nothing else:
{"title": "...", "summary": "...", "language": "...", "script": "...", "sources": [{"title": "...", "url": "...", "published": "YYYY-MM-DD"}]}
- "title": the episode title, in the requested language, no date prefix.
- "summary": 2-4 sentences describing the episode, in the requested language.
- "language": the BCP-47 primary tag of the language the script is actually written in, e.g. "en" or "es". Report it honestly — it is checked against the request.
- "script": the full spoken text.
- "sources": every source that informed the episode, with its publication date when known.
Do not put any text outside the fenced block in that final message.`

// userMessage is the per-session task: the request parameters the form
// collected, resolved into concrete instructions.
func userMessage(topic string, lengthMinutes, freshnessDays int, language string, now time.Time) string {
	words := lengthMinutes * wordsPerMinute
	return fmt.Sprintf(
		"Today is %s.\nTopic: %s\nTarget length: about %d spoken words (a %d-minute episode).\nFreshness window: the last %d days.\nLanguage: %s\n\nResearch the topic and produce the episode as specified in your instructions.",
		now.UTC().Format("Monday, 2 January 2006"), topic, words, lengthMinutes, freshnessDays, languageName(language),
	)
}

// translateMessage is the follow-up task sent when the agent's script
// came back in the wrong language: translate in place, same contract.
func translateMessage(language string) string {
	name := languageName(language)
	return fmt.Sprintf(
		"The episode must be entirely in %s, but your script is in a different language. Translate the episode now: title, summary, and script all in %s. Keep the sources exactly as they are. Reply with the full JSON contract again in a single fenced json block, including the corrected \"language\" field.",
		name, name,
	)
}

func languageName(code string) string {
	switch code {
	case "es":
		return "Spanish (español)"
	default:
		return "English"
	}
}

// Slugify turns a Topic into the slug's topic part: lowercase, runs of
// anything else collapsed to single dashes, capped so the date prefix
// plus a collision suffix still reads well.
func Slugify(topic string) string {
	var b strings.Builder
	dash := true // no leading dash
	for _, r := range strings.ToLower(topic) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "episode"
	}
	return s
}
