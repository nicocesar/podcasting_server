package mix

// Levels: what the mixer measured, and what it decided to do about it.
//
// The idea this file exists to serve is that no level in an episode is an
// absolute number. Everything is placed relative to the narration in that
// same episode: the bed sits BedGainDB under it, an effect sits EffectGainDB
// under it, and the finished file is moved so the narration lands on
// TargetLUFS. Nothing here has to know what ElevenLabs happens to return
// this month, which is the whole point — the previous arrangement put the
// bed BedGainDB under an unknown and called it "under the narration".
//
// None of this file shells out. The arithmetic is separated from the ffmpeg
// calls in mix.go on purpose: every test that needs ffmpeg skips on a
// machine without it, so the rules that decide how loud an episode is would
// otherwise be the least-tested code in the package rather than the most.

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// TargetLUFS is where the narration lands in the finished file. -16 is
	// the podcast convention (Apple and Spotify both sit near it), so an
	// episode plays at the same volume as whatever preceded it in someone's
	// queue.
	TargetLUFS = -16.0

	// TruePeakCeilingDB is the highest sample the finished file may reach.
	// -1 rather than 0 leaves room for the peaks a lossy encoder invents
	// between samples, which is why it is a true-peak ceiling and not a
	// sample-peak one.
	TruePeakCeilingDB = -1.0

	// BedGainDB is how far under the narration the music bed sits.
	//
	// This now measures what it says. It was chosen when it could not: with
	// the bed arriving at an unknown level, the only safe setting was one
	// low enough to stay out of the way of the quietest line in the story,
	// which is why it is well down rather than merely down. It is kept at
	// that value through this change deliberately — moving a constant in
	// the same commit that changes what the constant means would leave no
	// way to attribute the result. The story.levels trace says whether to
	// raise it.
	BedGainDB = -18.0

	// EffectGainDB is how far under the narration a sound effect sits. Not
	// as far as the bed: an effect is meant to be noticed, it is just not
	// meant to be the loudest thing in a bedtime story.
	EffectGainDB = -3.0

	// EffectPeakHeadroomDB caps an effect against the loudest thing the
	// narrator said. Averages alone do not stop a short, sharp effect from
	// stabbing through a quiet story — a thunderclap can measure modestly
	// and still be the only startling moment in it — so an effect is held
	// to both a average and a peak, whichever binds harder.
	EffectPeakHeadroomDB = 0.0

	// SilenceFloorLUFS is where a measurement stops being believable.
	// Below this the narration is silence, a failed render, or a broken
	// measurement, and the gain that would "correct" it is large enough to
	// destroy the episode. See unmeasured.
	SilenceFloorLUFS = -60.0

	// MaxBoostDB bounds how far the finished file may be lifted. The peak
	// ceiling already prevents clipping, but it does not prevent politely
	// amplifying the hiss of a render that came back quiet because
	// something was wrong with it.
	//
	// Placed to catch broken renders without second-guessing a merely
	// quiet vendor: speech at -34 LUFS still reaches the target, which is
	// well below anything a text-to-speech API plausibly returns, while a
	// render down at -50 is held to a shrug instead of being amplified
	// into a loud failure. When it binds, the trace says so — an episode
	// that missed the target should not do it silently.
	MaxBoostDB = 18.0
)

// Bed fades. The bed is the only thing that can fade without eating words:
// the last thing a listener hears is speech, so a fade over the mix itself
// would swallow "goodnight".
const (
	// BedFadeIn is long enough that the music arrives rather than appears.
	BedFadeIn = 2 * time.Second
	// BedFadeOut runs under the closing lines, which is safe because it
	// touches only the bed — the speech above it stays at full level.
	BedFadeOut = 4 * time.Second
	// BedTail is how long the music plays on after the last word, so a
	// story ends rather than stops.
	BedTail = 3 * time.Second
)

// measurement is one thing ffmpeg looked at.
//
// Two tools fill this in, because one metric does not fit everything here.
// LUFS is gated loudness: it discards the gaps between words, which is what
// makes it the right way to compare minutes of narration against a minute
// of continuous music, and useless on a 1.5s duck quack where the gate has
// nothing to gate. Effects are therefore compared by mean and peak, and
// speech is measured both ways so each comparison happens in matching units.
type measurement struct {
	LUFS float64 // integrated loudness, loudnorm input_i
	Peak float64 // true peak dBFS, loudnorm input_tp
	Mean float64 // volumedetect mean_volume
	Max  float64 // volumedetect max_volume

	// HasLoudness and HasVolume record which tools actually reported, so a
	// half-failed measurement is never mistaken for a measured zero.
	HasLoudness bool
	HasVolume   bool
}

// usable reports whether a narration measurement can be trusted to place
// everything else against. A measurement below the silence floor is not a
// quiet episode to be corrected, it is a broken one to be left alone.
func (m measurement) usable() bool {
	if !m.HasLoudness || !m.HasVolume {
		return false
	}
	if math.IsNaN(m.LUFS) || math.IsNaN(m.Peak) || math.IsNaN(m.Mean) || math.IsNaN(m.Max) {
		return false
	}
	return m.LUFS > SilenceFloorLUFS
}

// Levels is the record of one mix, for the trace. It is the only way to
// find out what a published episode actually did without listening to it,
// which matters most in exactly the situation this feature shipped in:
// constants chosen from first principles that nobody has heard yet.
type Levels struct {
	// Measured is false when the narration could not be measured and the
	// mixer fell back to leaving every level alone.
	Measured bool
	// Note says why, when Measured is false. An episode that came out flat
	// should not require reading the mixer to find out what happened.
	Note string

	SpeechLUFS float64
	SpeechPeak float64
	SpeechMean float64
	SpeechMax  float64

	BedLUFS float64
	BedGain float64

	// EffectGains is in part order, one entry per effect part.
	EffectGains []float64

	FinalGain float64
	// FinalClamp names the rule that bound the final gain, if either did:
	// "peak" or "boost". Empty when the gain is simply what the target
	// asked for.
	FinalClamp string
}

// unmeasured is the fallback: what the mixer did before it measured
// anything. Every gain is zero, the bed keeps its flat attenuation, and the
// caller skips the fades and the tail.
//
// This is the same policy as the rest of the performed pipeline — a music
// bed the vendor would not compose is skipped, a sound effect that will not
// render is skipped, headers that will not strip are left alone — because a
// slightly wrong episode beats a failed one every time.
func unmeasured(note string) Levels {
	return Levels{Measured: false, Note: note, BedGain: BedGainDB}
}

// effectGain places one effect under the narration.
//
// Both rules are relative to the narration and the tighter one wins: sit
// EffectGainDB under it on average, and never peak above the loudest thing
// the narrator said. The second is what actually protects a sleeping child
// from a hot effect; the first is what keeps a timid one audible.
func effectGain(speech, fx measurement) float64 {
	if !fx.HasVolume || math.IsNaN(fx.Mean) || math.IsNaN(fx.Max) {
		return 0
	}
	byMean := speech.Mean + EffectGainDB - fx.Mean
	byPeak := speech.Max + EffectPeakHeadroomDB - fx.Max
	return math.Min(byMean, byPeak)
}

// bedGain places the bed under the narration. Both sides are gated
// loudness, so this compares music that plays continuously against speech
// with the pauses between words discounted — the comparison a listener
// actually makes.
func bedGain(speech, bed measurement) float64 {
	if !bed.HasLoudness || math.IsNaN(bed.LUFS) || bed.LUFS <= SilenceFloorLUFS {
		return BedGainDB
	}
	return speech.LUFS + BedGainDB - bed.LUFS
}

// finalGain moves the whole mix so the narration lands on TargetLUFS.
//
// One static gain, not a dynamic normaliser: everything underneath it has
// already been placed relative to the narration, so a single number
// preserves all of those relationships exactly. A dynamic pass would move
// them around, and a bed that breathes is a bed nobody asked for.
//
// The returned string names whichever guard bound the result, for the trace.
func finalGain(speech measurement) (float64, string) {
	g := TargetLUFS - speech.LUFS
	clamp := ""
	if g > MaxBoostDB {
		g, clamp = MaxBoostDB, "boost"
	}
	// The loudest sample in the mix is the narration's: effects are held at
	// or below its peak and the bed is far under it, so this is the peak
	// that decides whether the finished file clips.
	if speech.Peak+g > TruePeakCeilingDB {
		g, clamp = TruePeakCeilingDB-speech.Peak, "peak"
	}
	return g, clamp
}

// fades returns the bed's fade-in and fade-out for a story of this length,
// shrunk to fit when the story is too short to hold both. A story can be
// short — an agent that submits almost nothing still gets published, and
// the credit alone is only seconds — and fades that overlap each other
// produce a bed that never reaches its own level.
func fades(total time.Duration) (in, out time.Duration) {
	in, out = BedFadeIn, BedFadeOut
	if in+out <= total {
		return in, out
	}
	if total <= 0 {
		return 0, 0
	}
	// Keep their proportions rather than truncating one of them, so a very
	// short story still fades at both ends instead of only the first.
	scale := float64(total) / float64(in+out)
	return time.Duration(float64(in) * scale), time.Duration(float64(out) * scale)
}

// loudnormJSON is the subset of loudnorm's analysis report we read. It
// prints more, all of it about the correction it would apply, which is not
// wanted here: loudnorm is used as a meter, and the correcting is done by
// one static volume filter whose value this file decides.
type loudnormJSON struct {
	InputI  string `json:"input_i"`
	InputTP string `json:"input_tp"`
}

// parseLoudness reads integrated loudness and true peak out of an ffmpeg
// run with loudnorm=print_format=json.
//
// The JSON block is found by scanning for the last balanced object in the
// log rather than by locating the filter that emitted it. ffmpeg labels the
// line with the filter's position in the graph ("Parsed_loudnorm_5"), which
// is a number that moves whenever the graph changes shape — a fine way to
// write a parser that breaks quietly the next time this file is edited.
func parseLoudness(stderr string) (measurement, error) {
	start := strings.LastIndex(stderr, "{")
	end := strings.LastIndex(stderr, "}")
	if start < 0 || end < start {
		return measurement{}, fmt.Errorf("no loudnorm report in ffmpeg output")
	}
	var report loudnormJSON
	if err := json.Unmarshal([]byte(stderr[start:end+1]), &report); err != nil {
		return measurement{}, fmt.Errorf("loudnorm report: %w", err)
	}
	i, err := parseDB(report.InputI)
	if err != nil {
		return measurement{}, fmt.Errorf("loudnorm input_i: %w", err)
	}
	tp, err := parseDB(report.InputTP)
	if err != nil {
		return measurement{}, fmt.Errorf("loudnorm input_tp: %w", err)
	}
	return measurement{LUFS: i, Peak: tp, HasLoudness: true}, nil
}

var (
	meanRE = regexp.MustCompile(`mean_volume:\s*(-?[\d.]+|-?inf|nan)\s*dB`)
	maxRE  = regexp.MustCompile(`max_volume:\s*(-?[\d.]+|-?inf|nan)\s*dB`)
)

// parseVolume reads mean and peak out of an ffmpeg run with volumedetect.
func parseVolume(stderr string) (measurement, error) {
	mean := meanRE.FindStringSubmatch(stderr)
	max := maxRE.FindStringSubmatch(stderr)
	if mean == nil || max == nil {
		return measurement{}, fmt.Errorf("no volumedetect report in ffmpeg output")
	}
	mv, err := parseDB(mean[1])
	if err != nil {
		return measurement{}, fmt.Errorf("volumedetect mean_volume: %w", err)
	}
	xv, err := parseDB(max[1])
	if err != nil {
		return measurement{}, fmt.Errorf("volumedetect max_volume: %w", err)
	}
	return measurement{Mean: mv, Max: xv, HasVolume: true}, nil
}

// parseDB reads one decibel figure. Digital silence reports as "-inf", and
// Go parses that into a real negative infinity, which then compares
// correctly against the silence floor; "nan" also parses, to a value that
// compares false against everything, which is why usable tests for it by
// name rather than by comparison.
func parseDB(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// merge folds a second tool's reading of the same audio into the first.
func (m measurement) merge(other measurement) measurement {
	if other.HasLoudness {
		m.LUFS, m.Peak, m.HasLoudness = other.LUFS, other.Peak, true
	}
	if other.HasVolume {
		m.Mean, m.Max, m.HasVolume = other.Mean, other.Max, true
	}
	return m
}
