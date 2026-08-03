package generation

// The Generation Template registry. A Template is a code-defined bundle:
// the program's branding on /me/generate, its own platform agent (name +
// system prompt + tools — each persona versions independently, ADR 0009),
// the form fields it collects, and the task message it sends. The
// pipeline (script → TTS → publish) is shared; adding a template is one
// entry here plus its prompt.

import (
	"fmt"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// Template is one program the station can produce.
type Template struct {
	ID      string // path segment and stored identifier
	Name    string // program name on the chooser card
	Tagline string // one-line pitch under the name

	AgentName    string // platform agent; one persona per template
	SystemPrompt string
	Tools        []map[string]any

	// SubmitToolName is the tool the agent calls to deliver its work.
	// Per-template because the deliverable differs: the spoken programs
	// hand back prose (submit_episode), the ambient program hands back a
	// composition plan (submit_music).
	SubmitToolName string

	// RequiresSources rejects a submission that cites nothing. True for
	// the programs built on research, where an empty list means the agent
	// skipped the work. False for Story Time, whose own prompt tells it to
	// submit an empty list when it invented the tale — rejecting that
	// would loop the agent against its own instructions until the session
	// timed out.
	RequiresSources bool

	// IsMusic marks a template whose audio is composed rather than
	// voiced. It routes the runner past TTS entirely, and through
	// HasVoicePicker it takes the voice and provider fields off the form —
	// there is no speech in the output for them to describe.
	IsMusic bool

	// NeedsDialogue marks a template whose audio is a performance rather
	// than a reading: several voices, sound effects, a music bed, mixed
	// together. Only ElevenLabs can render it, so this template does not
	// get the free engine chain ADR 0009 gives everything else — it needs
	// a key, and without one it refuses to start rather than quietly
	// producing the flat single-voice version it exists to replace.
	//
	// It also routes the runner past voiceAndPublish entirely: the
	// deliverable is a segment list, not prose.
	NeedsDialogue bool

	// Which form fields the template collects beyond the shared set
	// (topic, length, language, voice, provider).
	HasFreshness      bool
	HasAgeRange       bool
	HasCast           bool // returning-characters picker
	HasSaveCharacters bool // "save the characters" checkbox
	// HasTargetLanguage adds the second language select: the one the
	// listener is practicing, as opposed to the one the story is told in.
	HasTargetLanguage bool

	// DerivesInterval marks a template whose Beat cadence comes from the
	// Freshness Window instead of its own select: firing exactly as often
	// as the window is wide means consecutive Episodes neither re-cover
	// nor skip ground. It implies HasFreshness, and it is why a Timeless
	// briefing cannot become a Beat at all — no window, no cadence.
	DerivesInterval bool

	// TopicLabel and TopicPlaceholder brand the shared textarea.
	TopicLabel       string
	TopicPlaceholder string

	// ProgressTitle heads the progress page, and PlanStage/AudioStage
	// name the first two pipeline stages on it. The stages themselves are
	// the same for every program, but what happens inside them is not:
	// the spoken programs research and voice a script, the ambient one
	// plans and renders a piece. A listener watching the page should read
	// about the program they actually asked for.
	ProgressTitle string
	PlanStage     string
	AudioStage    string

	// TaskMessage renders the per-session task from the submitted
	// request. It must be a pure function of (g, now) so a resumed
	// Generation rebuilds the identical message (ADR 0009).
	TaskMessage func(g store.Generation, now time.Time) string
}

// TemplateIDs is the chooser order. A new program is one entry in
// templates plus an ID here.
var TemplateIDs = []string{"news", "stories", "stories-v2", "ambient"}

var templates = map[string]Template{
	"news": {
		ID:              "news",
		Name:            "The Briefing",
		Tagline:         "An agent researches your topic on the web and reads you the news.",
		AgentName:       agentName,
		SystemPrompt:    systemPrompt,
		Tools:           append(agentTools[:len(agentTools):len(agentTools)], submitTool),
		SubmitToolName:  submitToolName,
		RequiresSources: true,
		HasFreshness:    true,
		DerivesInterval: true,
		TopicLabel:      "Topic",
		ProgressTitle:   "Generating an episode",
		PlanStage:       "Researching & writing the script",
		AudioStage:      "Voicing",
		TopicPlaceholder: "e.g. developments in fusion energy — or a whole brief: " +
			"angle, things to include, tone…",
		TaskMessage: func(g store.Generation, now time.Time) string {
			return userMessage(g.Topic, g.LengthMinutes, g.FreshnessDays, g.Language, now)
		},
	},
	"stories": {
		ID:                "stories",
		Name:              "Story Time",
		Tagline:           "A new tale, told just for your kids — with characters that can come back.",
		AgentName:         "podcasting-storyteller",
		SystemPrompt:      storiesSystemPrompt,
		Tools:             append(agentTools[:len(agentTools):len(agentTools)], submitTool),
		SubmitToolName:    submitToolName,
		HasAgeRange:       true,
		HasCast:           true,
		HasSaveCharacters: true,
		TopicLabel:        "Story idea",
		ProgressTitle:     "Generating a story",
		PlanStage:         "Writing the story",
		AudioStage:        "Voicing",
		TopicPlaceholder: "e.g. a dragon who is afraid of heights learns to trust her wings — " +
			"or a whole brief: characters, setting, the lesson, tone…",
		TaskMessage: storiesMessage,
	},
	"stories-v2": {
		ID:      "stories-v2",
		Name:    "Story Time Studio",
		Tagline: "A story performed, not read: several voices, sound effects, music — and a second language woven through it.",

		// Its own agent, not a second version of the storyteller's. The
		// two personas write different things and their prompts have to
		// version independently (ADR 0011).
		AgentName:      "podcasting-storyteller-v2",
		SystemPrompt:   storiesV2SystemPrompt,
		Tools:          append(agentTools[:len(agentTools):len(agentTools)], submitStoryTool),
		SubmitToolName: submitStoryToolName,
		NeedsDialogue:  true,

		HasAgeRange:       true,
		HasCast:           true,
		HasSaveCharacters: true,
		HasTargetLanguage: true,

		TopicLabel: "Story idea",
		TopicPlaceholder: "e.g. a duck and a pig who move into an empty barn — " +
			"or a whole brief: characters, setting, the lesson, tone…",
		ProgressTitle: "Producing a story",
		PlanStage:     "Writing the story",
		AudioStage:    "Performing",
		TaskMessage:   storiesV2Message,
	},
	"ambient": {
		ID:      "ambient",
		Name:    "The Long Room",
		Tagline: "Instrumental music composed to order — for sleeping, working, or doing nothing at all.",

		AgentName:    "podcasting-composer",
		SystemPrompt: ambientSystemPrompt,
		// No web tools: there is nothing to research. The composer's
		// whole job is to turn a mood into a plan.
		Tools:          []map[string]any{submitMusicTool},
		SubmitToolName: submitMusicToolName,
		IsMusic:        true,

		TopicLabel: "Mood",
		TopicPlaceholder: "e.g. rain on a window, late evening — " +
			"or a whole brief: instruments, tempo, how it should end…",
		ProgressTitle: "Composing your music",
		PlanStage:     "Planning the composition",
		AudioStage:    "Composing",
		TaskMessage:   ambientMessage,
	},
}

// HasVoicePicker reports whether the form should offer the narrator's
// gender and the TTS provider.
//
// Both are meaningless for the programs that do not have a single narrator
// to describe. A composed piece has no speech at all; a performed story
// casts every line from its role, and its provider is decided by the one
// vendor that can render dialogue. Offering either would be offering a
// control that does nothing.
func (t Template) HasVoicePicker() bool { return !t.IsMusic && !t.NeedsDialogue }

// CarriesCast reports whether Episodes from this template can hold a cast
// of Characters — both to extract one onto, and to offer as a returning
// cast on a later Generation.
//
// A predicate rather than a literal "stories" comparison because there is
// now more than one storytelling program, and a hard-coded ID would mean
// Story Time Studio silently could not reuse its own characters, which is
// the feature people notice.
func CarriesCast(templateID string) bool {
	tpl, ok := TemplateByID(templateID)
	return ok && tpl.HasSaveCharacters
}

// LanguageLabel brands the language select. It is deliberately narrower
// for music: the language shapes the title and summary a person reads in
// their feed, and nothing whatsoever about the audio — the composer is
// never told it, and the Music API is never sent it.
func (t Template) LanguageLabel() string {
	if t.IsMusic {
		return "Title & summary language"
	}
	if t.HasTargetLanguage {
		// With two language selects on the form, "Output language" stops
		// being self-explanatory: both are output.
		return "Told in"
	}
	return "Output language"
}

// TemplateByID resolves id, treating "" as news: every Generation that
// predates the Template field was a news briefing.
func TemplateByID(id string) (Template, bool) {
	if id == "" {
		id = "news"
	}
	t, ok := templates[id]
	return t, ok
}

// AgeRangeOption is one listener age band on the stories form.
type AgeRangeOption struct {
	Value string
	Label string
}

var AgeRanges = []AgeRangeOption{
	{"2-4", "Ages 2–4"},
	{"5-7", "Ages 5–7"},
	{"8-12", "Ages 8–12"},
	{"all", "All ages"},
}

// ValidAgeRange reports whether v is one of the offered bands.
func ValidAgeRange(v string) bool {
	for _, o := range AgeRanges {
		if o.Value == v {
			return true
		}
	}
	return false
}

// ageRangePhrase turns the stored band into task-message prose.
func ageRangePhrase(v string) string {
	switch v {
	case "2-4":
		return "children aged 2 to 4 (very simple words, short sentences, lots of repetition and sound)"
	case "5-7":
		return "children aged 5 to 7 (simple vocabulary, a clear beginning-middle-end, gentle stakes)"
	case "8-12":
		return "children aged 8 to 12 (richer vocabulary and plot welcome; keep it age-appropriate)"
	default:
		return "a family audience of all ages — enchanting for children, charming for grown-ups"
	}
}

// storiesSystemPrompt is the Story Time agent's behavior. Like the news
// prompt it lives in the repo on purpose: the startup bootstrap pushes it
// to the platform, where a change becomes a new agent version (ADR 0009).
const storiesSystemPrompt = `You are the storyteller for a private podcast service that produces audio stories for children. Each task message gives you a story idea, the listeners' age range, a target spoken length in words, a language, and sometimes a returning cast of characters. Your job: write a complete, ready-to-voice children's story.

Story rules:
- Invent freely: the story is fiction and does not need research. But when the idea touches real facts — animals, space, history, how things work — use web search to get those facts right; children deserve truthful details woven into the tale.
- Write for the given age range: match its vocabulary, sentence length, pacing, and emotional stakes. Nothing frightening or inappropriate for the age.
- When a returning cast is given, those characters appear in this story too: keep their names, personalities, and details consistent with their descriptions, and let them grow a little.
- Give the story a satisfying shape: a warm opening, a real (age-sized) problem, and a gentle, hopeful ending. A light lesson may emerge naturally; never preach it.

Writing rules:
- Write in the requested language, and only that language.
- The story is read aloud by a single narrator: plain flowing prose. No markdown, no headings, no bullet points, no URLs, no stage directions, nothing a voice cannot speak. Character dialogue is fine as spoken prose.
- Open by inviting the listener in; close with a soft, sleep-friendly sign-off.
- Hit the target word count within about ten percent. Do not pad; if the tale is told, let it breathe with detail and feeling instead.

Output contract:
When the story is ready, deliver it by calling the submit_episode tool exactly once, filling every field as its schema describes. The summary should tell a parent what the story is about; list sources only if web research actually informed the story, otherwise submit an empty sources list. Never paste the story text, or any JSON version of it, into a chat message — only the tool call counts as delivery.
Before you submit, make sure the script field holds the finished story: the complete text at full length, never a placeholder, an outline, a summary, or a draft you meant to fill in later. The submission is final — it is voiced and published as sent, and there is no second call to correct it.
If the tool result rejects the submission, it explains what is wrong: fix exactly that and call submit_episode again with the full corrected story.`

// ambientSystemPrompt is the composer agent's behavior. Like the other
// prompts it lives in the repo so the boot bootstrap can push it, where a
// change becomes a new agent version (ADR 0009).
const ambientSystemPrompt = `You are the composer for a private podcast service that produces instrumental music. Each task message gives you a mood, a target length, and a language. Your job: plan a single continuous piece of music of that length, and deliver it as an ordered list of movements.

How the music is made:
- Each movement you write is rendered separately by a music generation model, then the movements are played back-to-back with no gap. A movement's prompt is the only thing that model sees — it has no memory of the movements before or after it.
- A single movement can be at most 10 minutes (600000 ms) and at least 3 seconds (3000 ms). This is a hard limit of the renderer. Any piece longer than 10 minutes must therefore be split across several movements.
- Prefer movements of 5 to 10 minutes. Many short movements make the piece feel restless and choppy.

Writing the movement prompts:
- Write each prompt for a music model, not for a listener: name instruments, tempo in bpm, key or tonality, texture, production character, and what the movement should not contain. Be concrete and sensory.
- Everything is instrumental. Never ask for vocals, lyrics, singing, spoken word, or any voice.
- Carry the piece's identity through every movement: repeat the core instrumentation, tempo, and key in each prompt so consecutive movements sound like one work rather than a shuffled playlist.
- Let the piece move. Across the movements, develop something — an instrument enters, the texture thickens, the tempo settles, the piece resolves. A long track that never changes is boring; one that changes at random is jarring.
- Respect the mood above all. If the request is for sleep or focus, avoid sudden dynamics, percussion hits, and anything that pulls attention.

Length rules:
- The movement durations must add up to the requested total, within about ten percent.
- Choose the split yourself. A 25-minute piece might be three movements; a 60-minute piece might be six or seven.

Output contract:
When the piece is planned, deliver it by calling the submit_music tool exactly once, filling every field as its schema describes. The title and summary are for a human browsing their feed: write them in the requested language, and describe the music, not the process. Never paste the plan, or any JSON version of it, into a chat message — only the tool call counts as delivery.
If the tool result rejects the submission, it explains what is wrong: fix exactly that and call submit_music again with the full corrected plan.`

// ambientMessage renders the composer's task. There is no word budget
// here — the length is a wall-clock duration the movements must add up
// to, which is the one number the agent most often gets wrong.
func ambientMessage(g store.Generation, now time.Time) string {
	return fmt.Sprintf(
		"Mood: %s\nTarget length: %d minutes (%d ms) total across all movements.\nLanguage for the title and summary: %s\n\nPlan the piece and submit it as specified in your instructions.",
		g.Topic, g.LengthMinutes, g.LengthMinutes*60*1000, languageName(g.Language),
	)
}

// storiesMessage renders the Story Time task: the request parameters the
// form collected, resolved into concrete instructions.
func storiesMessage(g store.Generation, now time.Time) string {
	words := g.LengthMinutes * wordsPerMinute
	var b strings.Builder
	fmt.Fprintf(&b, "Today is %s.\nStory idea: %s\nListeners: %s.\nTarget length: about %d spoken words (a %d-minute story).\nLanguage: %s\n",
		now.UTC().Format("Monday, 2 January 2006"), g.Topic, ageRangePhrase(g.AgeRange), words, g.LengthMinutes, languageName(g.Language))
	if len(g.Cast) > 0 {
		b.WriteString("\nReturning characters — reuse these characters and keep them consistent:\n")
		for _, c := range g.Cast {
			fmt.Fprintf(&b, "- %s — %s\n", c.Name, c.Description)
		}
	}
	b.WriteString("\nWrite the story and produce the episode as specified in your instructions.")
	return b.String()
}

// storiesV2Message renders the Story Time Studio task. Same request as
// storiesMessage plus the practiced language, and it names the base
// language as the base rather than as "the language" — the distinction is
// the whole point of the template, and the task message is where the agent
// first meets it.
func storiesV2Message(g store.Generation, now time.Time) string {
	words := g.LengthMinutes * wordsPerMinute
	var b strings.Builder
	fmt.Fprintf(&b, "Today is %s.\nStory idea: %s\nListeners: %s.\nTarget length: about %d spoken words (a %d-minute story).\nTell the story in: %s\n",
		now.UTC().Format("Monday, 2 January 2006"), g.Topic, ageRangePhrase(g.AgeRange), words, g.LengthMinutes, languageName(g.Language))
	if t := PrimaryTag(g.TargetLanguage); t != "" && t != PrimaryTag(g.Language) {
		fmt.Fprintf(&b, "Language being practiced: %s — weave it through the story, spoken by a native voice.\n", languageName(t))
	} else {
		b.WriteString("Language being practiced: none — tell the whole story in one language.\n")
	}
	if len(g.Cast) > 0 {
		b.WriteString("\nReturning characters — reuse these characters and keep them consistent:\n")
		for _, c := range g.Cast {
			fmt.Fprintf(&b, "- %s — %s\n", c.Name, c.Description)
		}
	}
	b.WriteString("\nWrite the story and produce it as specified in your instructions.")
	return b.String()
}
