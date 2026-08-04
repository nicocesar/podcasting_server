# 33. Every level is relative to the narration

Date: 2026-08-04

## Status

Accepted (closes the gap ADR 0032 deferred)

## Context

ADR 0032 shipped `internal/mix` with an explicit, named gap:

> **Levels and rhythm are deliberately not solved.** There is no loudness
> normalisation, no ducking of the bed under speech, no fade-out, and the
> agent is not directed to place pauses for pacing. The bed sits at a fixed
> −18dB and everything else arrives at whatever level the vendor returned.

Three consequences of that had hardened into the code.

**`BedGainDB = -18` measured nothing.** Its own comment said "how far under
the narration the music bed sits", but nothing measured the narration and
nothing measured the bed. It was 18dB under whatever the music model
happened to return that afternoon. The constant was set that low precisely
because it was defending against an unknown — it had to clear the quietest
line in any story — so the honest reading is that it was a guess sized for
the worst case rather than a relationship.

**A hot effect stabbed through the story.** `eleven_text_to_sound_v2`
returns a thunderclap and a yawn at unrelated levels and both went in
untouched. ADR 0032 named this as the thing most likely to hurt: "a
generated effect that comes back hot will be loud in a story meant for a
sleeping child".

**The bed slammed in at t=0 and stopped dead on the last word**, because
`amix duration=first` cut it there. A bedtime story ended by stopping.

And a pause the agent wrote was validated by `ParseStorySubmission`,
planned into a `Piece`, and then discarded twice — once by `renderPiece`,
which returned nothing for it, and again by an emptiness test in
`performAndPublish`. The schema said pauses were real; the audio never
contained one.

The work was approached without having listened to a batch of real
episodes, which shaped every decision below toward things that are right by
construction over constants tuned by ear.

## Decision

**Every level is a measured relationship to the narration in the same
episode.** The mixer meters before it places anything, and the constants
became offsets rather than absolutes: the bed sits `BedGainDB` under the
narration, an effect sits `EffectGainDB` under it, and one static gain moves
the finished file so the narration lands on `TargetLUFS` (−16, the podcast
convention). Nothing needs to know what ElevenLabs returns this month, and
the arrangement self-corrects if that changes.

**Speech is never leveled.** `eleven_v3` audio tags are performance
direction; a story where `[whispers] goodnight` is normalised to the same
loudness as a shout is a story whose direction has been thrown away. Speech
sets the reference and is copied through untouched.

**Two metrics, each where it is valid.** LUFS is gated loudness — it
discards the gaps between words, which is what makes it the right way to
compare minutes of narration against a minute of continuous music, and
useless on a 1.5s duck quack where the gate has nothing to gate. So the
speech↔bed relationship is in LUFS, and effects are held to mean and peak
from `volumedetect`. Speech is measured both ways so each comparison happens
in matching units.

**An effect is held to whichever rule binds harder** — `EffectGainDB` under
the narration on average, and never peaking above the loudest thing the
narrator said. The peak rule is what actually protects a sleeping child;
averages alone do not stop something short and sharp.

**The bed fades in, plays past the last word, and fades away.** The tail is
`apad` on the *story* chain rather than an extension of the bed: the padded
story is still `amix`'s first input, so it still decides the length and the
bed is still cut back to it. The bed is the only thing that can fade without
eating words — the last thing a listener hears is speech, so a fade over the
mix itself would swallow "goodnight".

**Pauses render.** `anullsrc` in the filter graph, so a pause costs no input
file and no silent MP3 to carry around.

**Measurement happens at mix time, not at SFX cache time.** Both were on the
table. Caching normalised effects would pay for measurement once per effect
ever instead of once per episode, but it would put ffmpeg inside
`internal/sfx`, require a `cacheVersion` bump, and leave every already-cached
effect at its original level. Measuring in `internal/mix` keeps ffmpeg
knowledge in the one package that shells out to anything, fixes stored
effects retroactively, and costs a handful of short subprocess calls against
a pipeline whose bed compose alone takes minutes.

**A failed measurement is never fatal.** Below `SilenceFloorLUFS`, or when
the narration cannot be metered at all, the mixer falls back to exactly what
it did before: no gains, a flat −18dB bed, no fades, no tail — and says why
in `Levels.Note`. This is the same trade the rest of this pipeline already
makes for a bed the vendor would not compose and an effect that will not
render. A slightly wrong episode beats a failed one.

**The numbers are traced.** `story.levels` carries the measured narration
loudness and peak, the bed's loudness and applied gain, the range of effect
gains, the final gain, and which guard clamped it if either did.

## Considered options

**Ducking the bed under speech with `sidechaincompress`.** Rejected for now.
It has four interacting parameters whose failure mode is audible pumping, and
tuning them requires listening to episodes nobody has heard yet. With the bed
now a measured distance under the narration rather than a hopeful one, a
static offset is the honest version of the same idea. This is the obvious
next step once there is listening evidence.

**A fixed absolute target for effects and bed** (say, effects peak at −12
dBFS). Simpler and stable episode-to-episode, but the numbers would have been
guesses about what ElevenLabs returns, made by someone who had not listened,
and they would rot silently if the vendor changed.

**Two-pass `loudnorm` on the finished mix.** Textbook-correct, but it costs a
third pass over the whole episode and its dynamic stage moves things around.
Everything under the final gain has already been placed relative to the
narration, so one static gain preserves all of those relationships exactly.
`loudnorm` is still used — as a meter, in analysis mode, never as a corrector.

**Normalising speech part-by-part.** Rejected: see above. It would also make
the seams between dialogue requests audible whenever the vendor's natural
level drifted.

**Raising `BedGainDB` in the same change.** Tempting, since −18 was only that
conservative to survive an unknown. Not done: changing what a constant means
and what it equals in one commit leaves no way to attribute the result. The
trace will say whether to raise it.

## Consequences

Story Time is louder and more consistent, and the bed finally sits where the
constant always claimed. Every published Story Time episode now lands at
−16 LUFS unless a guard says otherwise.

**This is Story Time only.** News and ambient still join audio by appending
raw MP3 frames and never invoke ffmpeg, so `mix.Available()` keeps its
meaning and the binary stays optional for them. A cross-program loudness pass
would make ffmpeg mandatory to publish anything, which is a bigger decision
than this one.

**`mix.Mix` returns a `Result`, not bytes**, so the levels can be traced.
`mix.Part` grew a `Kind`, whose zero value is `Speech` — a caller who forgets
to say gets the untouched behaviour rather than a processed one.

**The ffmpeg filter surface widened.** ADR 0032 tracked it at `mp3`, `amix`,
`concat`, `aresample`, `volume` against the possibility of compiling a
trimmed build. Add `loudnorm`, `volumedetect`, `afade`, `apad`, `anullsrc`
and `anull`. Still a small list, and still nothing from the video side, but
the trimmed-build estimate in ADR 0032 should be read as larger now.

**A mix is now several ffmpeg invocations rather than one**: one to meter the
narration, one per effect, one for the bed, one to build. Each meters seconds
of audio and the whole set is well under a second — immaterial next to a
60-second music generation and several ElevenLabs round trips, but it is no
longer true that mixing means one subprocess.

**The rules are tested without ffmpeg.** All the arithmetic lives in
`levels.go` and is covered by `levels_test.go`, which needs no binary. This
matters because every test in `mix_test.go` skips on a machine without
ffmpeg, so before this change a bare CI run exercised none of the mixer.

**Seam quality improves for free.** ADR 0032 noted that the packer prefers to
break where a sound effect covers the seam, and that a quiet story had
nowhere to hide one. Pauses now break runs *and* produce real silence, so a
seam can land inside a deliberate beat.

What is still not solved: the agent is not directed to place pauses for
pacing, so the rhythm work is only half done — the mechanism exists and is
used only when the agent happens to reach for it. Filling in the voice roster
remains listening work. And the constants here were chosen from first
principles; the trace exists so the next revision can be chosen from evidence.
