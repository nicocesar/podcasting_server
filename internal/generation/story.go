package generation

// Story Time Studio's half of the generation contract: a script that is a
// list of things to render rather than a block of prose.
//
// The old contract hands back one string and one voice, which is why the
// bilingual stories it produced were read by an English narrator doing an
// accent — the pipeline had nowhere to put the information that this word
// is Spanish and somebody else should say it. Here, every spoken segment
// carries its own role and its own language, and the server casts each one
// separately.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicocesar/podcasting_server/internal/sfx"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// Segment kinds.
const (
	SegSpeech = "speech"
	SegSFX    = "sfx"
	SegPause  = "pause"
)

// Story is the storyteller agent's output and the template's durable
// midpoint, stored as JSON in Generation.Script exactly as Script and
// Composition are.
type Story struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Language is the base language: the one the story is told in, the
	// one the Episode and its feed entry are in, and the one the spoken
	// credit uses. The practiced language is never this.
	Language string `json:"language,omitempty"`
	// Bed is one music prompt for the whole story, laid underneath at a
	// fixed low level. Empty means no music.
	Bed      string    `json:"bed,omitempty"`
	Segments []Segment `json:"segments"`
}

// Segment is one thing to render, in order.
type Segment struct {
	Kind string `json:"kind"`
	// Speaker is a role from the canon (tts.Roles), for speech only.
	Speaker string `json:"speaker,omitempty"`
	// Lang is the language of this segment's text, for speech only. It is
	// what makes code-switching work: a Spanish word inside an English
	// story is its own segment, in "es", spoken by a Spanish voice.
	Lang string `json:"lang,omitempty"`
	// Text may carry eleven_v3 audio tags in square brackets, e.g.
	// "[whispers] goodnight" — direction the vendor reads and the
	// listener never hears.
	Text string `json:"text,omitempty"`
	// Cue is a library name from sfx.Library, or free text describing a
	// sound, for sfx only.
	Cue string `json:"cue,omitempty"`
	// MS is the length of a pause, for pause only.
	MS int `json:"ms,omitempty"`
}

// SpokenText is the segment's words with the direction removed — what a
// listener actually hears, and therefore what length checks should count.
func (s Segment) SpokenText() string { return tts.StripAudioTags(s.Text) }

// SpokenWords counts the words a listener hears across the whole story.
func (st Story) SpokenWords() int {
	n := 0
	for _, s := range st.Segments {
		if s.Kind == SegSpeech {
			n += len(strings.Fields(s.SpokenText()))
		}
	}
	return n
}

// Description renders the Episode description. A story has no sources, so
// this is the summary alone — the method exists so Story, Script and
// Composition stay interchangeable at the publish step.
func (st Story) Description() string { return strings.TrimSpace(st.Summary) }

// Languages returns the distinct segment languages, in first-seen order.
func (st Story) Languages() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range st.Segments {
		if s.Kind != SegSpeech {
			continue
		}
		l := PrimaryTag(s.Lang)
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// submitStoryToolName is Story Time Studio's counterpart to
// submit_episode. A separate tool rather than a wider submit_episode: the
// two deliverables have nothing in common past title and summary, and the
// old template's agent must keep seeing exactly the schema it was
// versioned against.
const submitStoryToolName = "submit_story"

// pauseBounds keep a pause meaningful and bounded.
const (
	MinPauseMS = 100
	MaxPauseMS = 5000
)

// submitStoryTool is the platform definition. Pushed by EnsureAgent; a
// change here becomes a new agent version (ADR 0009).
var submitStoryTool = map[string]any{
	"type":        "custom",
	"name":        submitStoryToolName,
	"description": "Submit the finished story. Call this exactly once when the story is complete. Never paste the story into a chat message.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":    map[string]any{"type": "string", "description": "Story title, in the base language, no date prefix."},
			"summary":  map[string]any{"type": "string", "description": "2-4 sentences describing the story, in the base language."},
			"language": map[string]any{"type": "string", "description": `BCP-47 primary tag of the base language the story is told in, e.g. "en". Not the language being practiced.`},
			"bed": map[string]any{
				"type":        "string",
				"description": "One music prompt for the whole story, played quietly underneath from beginning to end. Describe instruments, tempo and mood for a music model, not for a listener. Leave empty for no music.",
			},
			"segments": map[string]any{
				"type":        "array",
				"description": "The story in order. Each entry is one thing to render: a line of speech, a sound effect, or a pause.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind": map[string]any{
							"type":        "string",
							"enum":        []string{SegSpeech, SegSFX, SegPause},
							"description": "What this segment is.",
						},
						"speaker": map[string]any{
							"type":        "string",
							"enum":        tts.RoleIDs(),
							"description": "Required for speech. Who says this line, chosen from the fixed cast: " + roleHints() + ".",
						},
						"lang": map[string]any{
							"type":        "string",
							"description": `Required for speech. BCP-47 primary tag of this line's language, e.g. "en" or "es". Must be either the base language or the language being practiced.`,
						},
						"text": map[string]any{
							"type": "string",
							"description": "Required for speech. The words to say. You may direct the delivery with audio tags in square brackets, e.g. " +
								`"[whispers] goodnight, little one" or "[giggling] quack quack!". Tags are performed, not spoken.`,
						},
						"cue": map[string]any{
							"type": "string",
							"description": "Required for sfx. Prefer one of the ready-made effects: " + strings.Join(sfx.LibraryNames(), ", ") +
								". If none fits, describe the sound in a few words instead.",
						},
						"ms": map[string]any{
							"type":        "integer",
							"description": fmt.Sprintf("Required for pause. Length of the silence in milliseconds, between %d and %d.", MinPauseMS, MaxPauseMS),
						},
					},
					"required": []string{"kind"},
				},
			},
		},
		"required": []string{"title", "summary", "language", "segments"},
	},
}

func roleHints() string {
	parts := make([]string, len(tts.Roles))
	for i, r := range tts.Roles {
		parts[i] = r.ID + " (" + r.Hint + ")"
	}
	return strings.Join(parts, "; ")
}

// ParseStorySubmission decodes and validates a submit_story tool input.
//
// The language rule is the interesting one, and it is the opposite of the
// old template's. There, one episode-level tag was compared against the
// request and a mismatch triggered a demand to translate — which would
// reject a deliberately bilingual story on principle. Here each segment is
// checked against the two languages the request actually allows, so
// code-switching is legal and a third language is not.
//
// Errors are written to be read by the agent: each says what to fix.
func ParseStorySubmission(input []byte, lengthMinutes int, base, target string) (Story, error) {
	var st Story
	if err := json.Unmarshal(input, &st); err != nil {
		return Story{}, fmt.Errorf("submission does not match the contract: %w", err)
	}
	if st.Title == "" || st.Summary == "" {
		return Story{}, fmt.Errorf("submission is missing title or summary")
	}
	if len(st.Segments) == 0 {
		return Story{}, fmt.Errorf("submission has no segments — the story must be a list of things to render")
	}

	base = PrimaryTag(base)
	target = PrimaryTag(target)

	// Every defect in one rejection: a bad submission tends to be wrong in
	// several ways at once, and reporting them one at a time costs a
	// round-trip per problem.
	var problems []string
	speech := 0
	for i, s := range st.Segments {
		switch s.Kind {
		case SegSpeech:
			speech++
			if strings.TrimSpace(s.SpokenText()) == "" {
				problems = append(problems, fmt.Sprintf("segment %d is speech with no words in it", i+1))
			}
			if !tts.ValidRole(s.Speaker) {
				problems = append(problems, fmt.Sprintf(
					"segment %d has speaker %q, which is not one of the available voices (%s)",
					i+1, s.Speaker, strings.Join(tts.RoleIDs(), ", ")))
			}
			switch l := PrimaryTag(s.Lang); {
			case l == "":
				problems = append(problems, fmt.Sprintf("segment %d does not say what language it is in", i+1))
			case l == base:
			case target != "" && l == target:
			default:
				problems = append(problems, fmt.Sprintf(
					"segment %d is in %q, but this story may only use %s",
					i+1, s.Lang, allowedLanguages(base, target)))
			}
		case SegSFX:
			if strings.TrimSpace(s.Cue) == "" {
				problems = append(problems, fmt.Sprintf("segment %d is a sound effect with no cue", i+1))
			}
		case SegPause:
			if s.MS < MinPauseMS || s.MS > MaxPauseMS {
				problems = append(problems, fmt.Sprintf(
					"segment %d is a %dms pause, outside the allowed %d-%dms", i+1, s.MS, MinPauseMS, MaxPauseMS))
			}
		default:
			problems = append(problems, fmt.Sprintf(
				"segment %d has kind %q, which is not one of %s, %s, %s", i+1, s.Kind, SegSpeech, SegSFX, SegPause))
		}
	}
	if speech == 0 {
		problems = append(problems, "the story has no spoken segments — it is sound effects only")
	}
	// Same floor and the same reasoning as ParseSubmission: this catches a
	// stub, not a style miss. Accepting one is unrecoverable, because it
	// is checkpointed and Retry resumes from it rather than from the agent.
	want := lengthMinutes * wordsPerMinute
	if got := st.SpokenWords(); want > 0 && float64(got) < float64(want)*lengthFloor {
		problems = append(problems, fmt.Sprintf(
			"the story is %d spoken words but the request is for about %d (a %d-minute story) — submit the whole story, not a placeholder or an outline",
			got, want, lengthMinutes))
	}
	// A story that never uses the practiced language is not the episode
	// that was asked for, and it is the exact failure this template exists
	// to fix — worth a rejection rather than a quiet delivery.
	if target != "" && target != base {
		used := false
		for _, l := range st.Languages() {
			if l == target {
				used = true
				break
			}
		}
		if !used {
			problems = append(problems, fmt.Sprintf(
				"no segment is in %s — this story is meant to practice it, so weave it through the story",
				languageName(target)))
		}
	}
	if len(problems) > 0 {
		return Story{}, fmt.Errorf("%s", strings.Join(problems, "; and "))
	}
	return st, nil
}

func allowedLanguages(base, target string) string {
	if target == "" || target == base {
		return languageName(base)
	}
	return languageName(base) + " and " + languageName(target)
}

// Piece is one renderable unit of the planned story: a run of dialogue, a
// sound effect, or a silence. Plan turns a segment list into these.
type Piece struct {
	Kind  string
	Turns []tts.DialogueTurn // Kind == SegSpeech
	Cue   string             // Kind == SegSFX
	MS    int                // Kind == SegPause
}

// Plan groups the story's segments into the pieces the runner renders.
//
// Consecutive speech becomes one dialogue request, so the vendor can match
// prosody across the speaker changes — which is the whole reason for using
// dialogue rather than voicing each line alone. A run is broken when it
// would exceed budget, because past that the vendor starts truncating.
//
// Where the break lands matters. Prosody does not carry across a request
// boundary, so a seam in the middle of a conversation is audible. Effects
// and pauses already end a run, which means the natural seams fall exactly
// where a sound is covering them. A run of pure speech longer than the
// budget has no such cover and is split at the nearest segment boundary —
// audible, and the known cost of not having the agent place pauses for
// rhythm.
func Plan(st Story, budget int) []Piece {
	if budget <= 0 {
		budget = tts.DialogueCharBudget
	}
	var (
		pieces  []Piece
		run     []tts.DialogueTurn
		runSize int
	)
	flush := func() {
		if len(run) > 0 {
			pieces = append(pieces, Piece{Kind: SegSpeech, Turns: run})
			run, runSize = nil, 0
		}
	}
	for _, s := range st.Segments {
		switch s.Kind {
		case SegSpeech:
			turn := tts.DialogueTurn{
				Role:     s.Speaker,
				Language: PrimaryTag(s.Lang),
				Text:     s.Text,
			}
			size := len([]rune(s.Text))
			// A single segment over budget still goes out on its own: it
			// cannot be split without cutting a sentence in half, and one
			// over-long request is better than a mangled one.
			if len(run) > 0 && runSize+size > budget {
				flush()
			}
			run = append(run, turn)
			runSize += size
		case SegSFX:
			flush()
			pieces = append(pieces, Piece{Kind: SegSFX, Cue: s.Cue})
		case SegPause:
			flush()
			pieces = append(pieces, Piece{Kind: SegPause, MS: s.MS})
		}
	}
	flush()
	return pieces
}

// storiesV2SystemPrompt is Story Time Studio's persona. Derived from
// storiesSystemPrompt, with the single-narrator rule — the one that made
// the old template's bilingual stories impossible to voice properly —
// replaced by casting, direction, and code-switching instructions.
const storiesV2SystemPrompt = `You are the storyteller for a private podcast service that produces audio stories for children. Each task message gives you a story idea, the listeners' age range, a target spoken length in words, the language to tell the story in, sometimes a second language the listener is practicing, and sometimes a returning cast of characters. Your job: write a complete, ready-to-produce children's story.

Unlike a plain script, your story is produced: it is performed by several voices, over sound effects and music. You write all of that.

How a story is built:
- The story is a list of segments in order. A segment is one line of speech, one sound effect, or one pause.
- Every speech segment names a speaker from the fixed cast and the language that line is in. The narrator carries the story; the other voices are the characters in it.
- Use the cast deliberately. A duck should not be read by the narrator, and a story where every line is the narrator is a story that did not need this program.

Writing rules:
- Write for the ear and for the age given. Short sentences. Concrete images. Repetition is good for small children — but vary how repeated lines are performed, so three quacks in a row are three different quacks and not the same one three times.
- Direct the performance with audio tags in square brackets inside the text: [whispers], [giggling], [excited], [sleepy], [gently]. They are performed, never spoken aloud. Use them where the delivery matters, not on every line.
- Sound effects are punctuation, not decoration. Prefer the ready-made effects listed in the tool schema, because those are known quantities; describe a new one only when nothing fits.
- Character dialogue is spoken prose. No markdown, no headings, no bullet points, no URLs, no stage directions in the text itself — the tags and the segment structure are where direction goes.
- Open by inviting the listener in; close with a soft, sleep-friendly sign-off.
- Hit the target spoken word count within about ten percent. Audio tags do not count as spoken words.

When the listener is practicing a second language:
- Tell the story in the base language, and weave the practiced language through it — a word, a phrase, a short line at a time, always in a segment of its own marked with that language.
- Put those segments in the mouth of the tutor voice, or of a character who belongs to that language. They are spoken by a native speaker, so write them as a native speaker would say them, not as a learner's phrasebook entry.
- Introduce a practiced word in context, let the story make its meaning obvious, and bring it back. Do not translate it flatly every single time.
- Only ever use the two languages you were given.

When the idea does not work as given:
- Nobody reads your chat messages. This session has no human in it, so a reply asking which direction to take, or offering a list of alternatives, is not an answer — it stalls the episode and it is thrown away.
- If the idea names a real, identifiable person, tell the story with an invented character instead. Keep the shape of the idea — the setting, the mood, the kind of figure — and replace the person with one of your own making.
- If the idea is wrong for the age range, or you would not write it as asked for any other reason, write the nearest story you can stand behind and submit that. Say in the summary what the story is, not what it is not.
- Either way you deliver a finished story. Adapting the idea is always the answer; asking about it never is.

Output contract:
When the story is ready, deliver it by calling the submit_story tool exactly once, filling every field as its schema describes. Never paste the story, or any JSON version of it, into a chat message — only the tool call counts as delivery.
Before you submit, make sure the segments hold the finished story at full length, never a placeholder, an outline, or a draft you meant to fill in later. The submission is final — it is produced and published as sent.
If the tool result rejects the submission, it explains what is wrong: fix exactly that and call submit_story again with the whole corrected story.`
