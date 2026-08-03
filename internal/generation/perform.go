package generation

// The performed-story pipeline: Story Time Studio's counterpart to
// voiceAndPublish and composeAndPublish.
//
// What makes it different from voicing is that the parts are not all
// speech and not all in one voice. A story arrives as an ordered list of
// pieces — runs of dialogue, sound effects, silences — each rendered by
// whatever knows how, and then mixed rather than concatenated, because a
// music bed plays underneath rather than after.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/nicocesar/podcasting_server/internal/mix"
	"github.com/nicocesar/podcasting_server/internal/sfx"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// bedMS bounds the music bed request. The bed is looped to length by the
// mixer, so this is how much distinct music gets composed, not how long
// the story is: a minute of it under a five-minute story loops five times,
// which is what a bed is supposed to do.
const bedMS = 60_000

// bedAndMixSteps are the two units of work that happen after the last
// piece is rendered: composing the bed, and mixing everything together.
const bedAndMixSteps = 2

// judgeStory answers one submit_story call, mirroring judgeSubmission and
// judgeComposition: accept and checkpoint, or reject with what to fix and
// let the caller keep polling. An already-answered call is re-judged
// without re-answering — the crash-recovery and awaiting-resubmission
// paths in one.
//
// There is no translation round here. The old template's language check
// compared one episode-level tag against the request and demanded a
// translation on a mismatch, which is precisely the behaviour that made a
// bilingual story impossible; ParseStorySubmission checks each segment
// against the two languages the request allows, and a violation comes back
// as an ordinary rejection alongside every other defect.
func (r *Runner) judgeStory(ctx context.Context, g store.Generation, use *ToolUse) (store.Generation, bool, error) {
	story, perr := ParseStorySubmission(use.Input, g.LengthMinutes, g.Language, g.TargetLanguage)
	if perr != nil {
		if !use.Answered {
			r.trace(&g, store.LevelNotice, "story.rejected",
				"submission rejected, asking for a resubmission", "reason", perr)
			reject := "Rejected: " + perr.Error() + ". Fix that and call submit_story again with the whole story."
			if err := r.api.SendToolResult(ctx, g.SessionID, use.ID, reject, true); err != nil {
				return g, false, fmt.Errorf("reject submission: %w", err)
			}
		}
		return g, false, nil
	}
	if !use.Answered {
		if err := r.api.SendToolResult(ctx, g.SessionID, use.ID, "Story received and accepted. You are done.", false); err != nil {
			return g, false, fmt.Errorf("acknowledge submission: %w", err)
		}
	}
	g, err := r.acceptStory(ctx, g, story)
	return g, true, err
}

// acceptStory checkpoints the Story into the same Script field the other
// templates use, for the same reason acceptComposition does: Retry keys
// off Script being non-empty to decide whether a failed Generation resumes
// at the audio stage or goes back to the agent, and the written story is
// the expensive part worth keeping.
func (r *Runner) acceptStory(ctx context.Context, g store.Generation, story Story) (store.Generation, error) {
	raw, err := json.Marshal(story)
	if err != nil {
		return g, err
	}
	r.recordSessionUsage(ctx, &g)
	r.trace(&g, store.LevelInfo, "story.accepted", "story accepted",
		"title", story.Title, "segments", len(story.Segments),
		"words", story.SpokenWords(), "languages", story.Languages())
	g.Script = string(raw)
	g.Stage = store.GenVoicing
	return g, r.store.PutGeneration(ctx, g)
}

// performAndPublish renders and mixes a story, then publishes it.
func (r *Runner) performAndPublish(ctx context.Context, g store.Generation) (store.Generation, error) {
	var story Story
	if err := json.Unmarshal([]byte(g.Script), &story); err != nil {
		return g, fmt.Errorf("stored story is corrupt: %w", err)
	}
	dialogue := tts.DialogueEngineOf(r.engines)
	// Checked here as well as at the chooser: a Generation can be resumed
	// on an instance configured differently from the one that took it, and
	// half-performing a story is worse than not starting.
	if dialogue == nil {
		return g, fmt.Errorf("this program needs a dialogue-capable voice engine (ElevenLabs); none is configured")
	}
	if r.sfx == nil {
		return g, fmt.Errorf("this program needs a sound-effects renderer; none is configured")
	}
	if !mix.Available() {
		return g, fmt.Errorf("this program needs ffmpeg to mix the audio; it is not available")
	}

	pieces := Plan(story, tts.DialogueCharBudget)

	// The credit leads rather than trails. Everywhere else it is appended,
	// which on a story that just said "sweet dreams" arrives as a tonal
	// jump-cut at the exact moment the point was to be winding down. Spoken
	// in the narrator's voice so it belongs to the same production.
	var parts []mix.Part
	if p, ok := r.creditPart(ctx, &g, story, dialogue); ok {
		parts = append(parts, p)
	}

	// The bed and the mix are counted as work, not left off the end.
	//
	// Counting pieces alone made the bar reach 100% and then sit there for
	// minutes: composing the bed can take as long as everything before it,
	// and the mix runs after that. The page said "Performing" with a full
	// bar and nothing moving, which is indistinguishable from a hang — and
	// was reported as one. Whatever the last thing to finish is, the
	// progress has to still be moving while it runs.
	g.Stage = store.GenVoicing
	g.VoicedChunks, g.TotalChunks = 0, len(pieces)+bedAndMixSteps
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return g, err
	}
	step := func() {
		g.VoicedChunks++
		if err := r.store.PutGeneration(ctx, g); err != nil {
			r.trace(&g, store.LevelWarn, "progress.checkpoint_failed", "progress checkpoint failed", "err", err)
		}
	}

	for i, p := range pieces {
		part, err := r.renderPiece(ctx, &g, p, dialogue)
		if err != nil {
			return g, fmt.Errorf("piece %d of %d (%s): %w", i+1, len(pieces), p.Kind, err)
		}
		if len(part.Audio) > 0 {
			parts = append(parts, part)
		}
		// Checkpoint per piece, the same granularity the voiced path uses:
		// the progress page moves, and a crash resumes from the stored
		// Story rather than from the agent.
		step()
	}
	if len(parts) == 0 {
		return g, fmt.Errorf("the story rendered no audio")
	}

	// Traced before it starts, not after: this is the single slowest call
	// in the pipeline, and a log that only says when it finished cannot
	// answer "what is it doing right now".
	if story.Bed != "" {
		r.trace(&g, store.LevelInfo, "bed.composing", "composing the music bed", "ms", bedMS)
	}
	bed := r.renderBed(ctx, &g, story)
	step()

	r.trace(&g, store.LevelInfo, "story.mixing", "mixing", "parts", len(parts), "bed", len(bed) > 0)
	mp3, err := mix.Mix(ctx, parts, bed)
	if err != nil {
		return g, fmt.Errorf("mixing: %w", err)
	}
	step()
	r.trace(&g, store.LevelInfo, "story.mixed", "story mixed",
		"pieces", len(pieces), "parts", len(parts), "bed", len(bed) > 0, "bytes", len(mp3))

	// The spoken text, tags stripped, is what the Strand classifier should
	// read: audio tags are direction, and a classifier reading "[giggling]"
	// is reading something no listener hears.
	g, ep, err := r.publishAudio(ctx, g, story.Title, story.Description(), spokenScript(story), mp3)
	if err != nil {
		return g, err
	}

	if g.SaveCharacters {
		r.extractCharacters(ctx, &g, ep, spokenScript(story))
	}
	return r.finish(ctx, g, ep.Slug)
}

// renderPiece turns one planned piece into audio. A sound effect that
// cannot be rendered is skipped rather than fatal: the story is written,
// paid for, and perfectly listenable without one bird call, and failing
// the whole episode over it would be a bad trade. A failed line of
// dialogue is not skippable — that is the story itself.
func (r *Runner) renderPiece(ctx context.Context, g *store.Generation, p Piece, dialogue tts.DialogueEngine) (mix.Part, error) {
	switch p.Kind {
	case SegSpeech:
		inputs, err := tts.CastDialogue(p.Turns)
		if err != nil {
			return mix.Part{}, err
		}
		audio, err := dialogue.SynthesizeDialogue(ctx, inputs)
		if err != nil {
			return mix.Part{}, err
		}
		g.DialogueRequests++
		g.TTSEngine = dialogue.Name()
		for _, t := range p.Turns {
			g.TTSCharacters += utf8.RuneCountInString(t.Text)
		}
		return mix.Part{Audio: audio, Label: fmt.Sprintf("dialogue (%d turns)", len(p.Turns))}, nil

	case SegSFX:
		res, err := r.sfx.Render(ctx, sfx.Cue{Name: p.Cue})
		if err != nil {
			r.trace(g, store.LevelWarn, "sfx.failed", "sound effect skipped", "cue", p.Cue, "err", err)
			return mix.Part{}, nil
		}
		if res.CacheHit {
			g.SFXCacheHits++
		} else {
			g.SFXGenerated++
		}
		return mix.Part{Audio: res.Audio, Label: "sfx " + p.Cue}, nil

	case SegPause:
		// Nothing renders silence today: pause segments are in the schema
		// but the agent is not directed to use them, and giving them real
		// duration is part of the deferred rhythm work. Dropping them
		// keeps the plan honest — a pause that produces no silence should
		// not pretend to be a part.
		return mix.Part{}, nil
	}
	return mix.Part{}, fmt.Errorf("unknown piece kind %q", p.Kind)
}

// creditPart voices the station credit in the story's narrator voice.
// Non-fatal throughout: losing the credit is not worth losing an episode
// that is already written and largely paid for.
func (r *Runner) creditPart(ctx context.Context, g *store.Generation, story Story, dialogue tts.DialogueEngine) (mix.Part, bool) {
	voice, ok := tts.RoleVoice("narrator", g.Language)
	if !ok {
		return mix.Part{}, false
	}
	credit := tts.Credit(dialogue.Name(), voice, g.UserID, r.host)
	if credit == "" {
		return mix.Part{}, false
	}
	audio, err := dialogue.SynthesizeDialogue(ctx, []tts.DialogueInput{{Text: credit, VoiceID: voice.Eleven}})
	if err != nil || len(audio) == 0 {
		r.trace(g, store.LevelNotice, "tts.credit_failed", "credit skipped", "err", err)
		return mix.Part{}, false
	}
	g.DialogueRequests++
	g.TTSCharacters += utf8.RuneCountInString(credit)
	return mix.Part{Audio: audio, Label: "credit"}, true
}

// renderBed composes the music the story plays over. Non-fatal: a story
// without music is a story, and the bed is the one part of the production
// nobody will miss if the vendor is having a bad afternoon.
func (r *Runner) renderBed(ctx context.Context, g *store.Generation, story Story) []byte {
	if story.Bed == "" || r.music == nil {
		return nil
	}
	audio, err := r.music.Compose(ctx, story.Bed, bedMS)
	if err != nil {
		r.trace(g, store.LevelWarn, "bed.failed", "music bed skipped", "err", err)
		return nil
	}
	g.MusicCalls++
	g.MusicMillis += bedMS
	g.MusicModel = r.music.Model()
	return audio
}

// extractCharacters is the cast extraction the form asked for, lifted out
// of voiceAndPublish so both spoken pipelines run the identical thing.
// Non-fatal by design: the Episode is already published, and the backfill
// button covers a missed extraction.
func (r *Runner) extractCharacters(ctx context.Context, g *store.Generation, ep store.Episode, script string) {
	chars, u, err := ExtractCharacters(ctx, r.api, script)
	// Extraction burned real tokens either way; fold them into the meters.
	g.InputTokens += u.InputTokens
	g.OutputTokens += u.OutputTokens
	g.CacheReadTokens += u.CacheReadTokens
	if err != nil {
		r.trace(g, store.LevelWarn, "characters.extraction_failed", "character extraction failed",
			"episode", ep.Slug, "err", err)
		return
	}
	ep.Characters = chars
	if err := r.store.UpdateEpisode(ctx, ep); err != nil {
		r.trace(g, store.LevelWarn, "characters.save_failed", "could not save characters",
			"episode", ep.Slug, "count", len(chars), "err", err)
		return
	}
	r.trace(g, store.LevelInfo, "characters.extracted", "characters extracted",
		"episode", ep.Slug, "count", len(chars), "names", characterNames(chars))
}

// spokenScript flattens the story to the words a listener hears, for the
// consumers that want prose: the Strand classifier and character
// extraction. Audio tags are stripped — they are direction, not dialogue.
func spokenScript(story Story) string {
	var b bytes.Buffer
	for _, s := range story.Segments {
		if s.Kind != SegSpeech {
			continue
		}
		if t := s.SpokenText(); t != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(t)
		}
	}
	return b.String()
}

// Compile-time proof that the concrete client satisfies the narrow
// interface the runner declares, without the runner importing it for any
// other reason.
var _ SFXRenderer = (*sfx.Client)(nil)
