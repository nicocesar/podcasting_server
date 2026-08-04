# 32. A program may require its vendor, and the server may mix

Date: 2026-08-02

## Status

Accepted (narrows ADR 0009's engine chain; extends ADR 0011's registry)

## Context

Story Time was asked for a bedtime story for a two-year-old practicing
Spanish. It produced a good script and bad audio, and the reasons were
all structural rather than a bad roll of the dice.

The script was English narration with Spanish words embedded — `vacía`,
`pato`, `chancho`. The pipeline has one `Generation.Language` and voices
an episode end-to-end in one voice, so those words were read by an English
narrator doing an accent. Worse, the server actively fought the attempt:
`judgeSubmission` compares the agent's self-reported language against the
request and, on a mismatch, demands a translation. A deliberately
bilingual story is not a mistake to be corrected, but the guardrail could
not tell the difference.

The delivery was flat. `eleven_multilingual_v2` takes only text and a
voice; there is no way to say that the second "quack" should differ from
the first. Repetition is right for toddlers — identical repetition is not.

And there was no production: no sound effects, no music. Audio is
assembled by appending MP3 frames, which is legal only because every
producer pins `mp3_44100_128`, and sufficient only while parts follow one
another in time. A music bed does not follow; it plays underneath. No
amount of byte-appending produces overlap, and the image had no mixer.

Finally, the spoken station credit is appended last on every episode. On a
story that has just said "close your soft eyes", a differently-pitched
credit is a jump-cut at precisely the wrong moment.

## Decision

**A new program, not a rewrite.** Story Time Studio is a fourth Generation
Template (`stories-v2`) beside the existing `stories`, which is untouched.
Old Beats, old Generations, and the free voicing path all keep working;
the new program is visibly the richer, paid one. This follows ADR 0011: a
program is a registry entry with its own platform agent
(`podcasting-storyteller-v2`), so the two storytellers' prompts version
independently.

**A template may require a vendor.** ADR 0009 gave every spoken program
the same deal: three interchangeable engines, edge-tts free and first,
ElevenLabs opt-in. A performed story cannot honour that — multi-speaker
dialogue, audio tags and generated effects exist at one vendor. So
`Template.NeedsDialogue` marks a program that needs more than `Engine`
provides, and such a program is **removed from the chooser** when its
dependencies are missing, exactly as a music template already is without a
composer. It never silently degrades: producing the flat single-voice
reading is the bug this program exists to fix, so shipping it as a
fallback would be shipping the bug.

This narrows ADR 0009 rather than superseding it. `Engine` and its chain
are unchanged and still carry news and Story Time.

**The script is a list, not a blob.** `submit_story` returns ordered
segments — speech, sound effect, pause — where each speech segment names a
role and its own language. That is what makes code-switching work: the
Spanish word is its own segment, in `es`, cast to a Spanish voice.
Validation checks each segment against the two languages the request
allows, so a bilingual story is legal and a third language is not. A story
that never uses the practiced language is rejected, because it is not the
episode that was asked for.

**Casting is a closed enum.** The agent picks a speaker from a fixed list
of roles (`narrator`, `tutor`, `small_squeaky`, …) and the server resolves
role + language to a voice. Same discipline as the Strand canon (ADR
0017): a model that cannot name a voice cannot invent one, misspell one,
or re-cast the duck halfway through. Roles with no curated voice fall back
through the existing table rather than failing.

**Effects are remembered.** A cue is rendered once and stored as a station
asset (`PutAsset`/`OpenAsset`, owned by no User and untouched by
`DeleteUser`). Curated cues are keyed by name, freeform ones by the hash
of their normalised text. This is a cost decision second and a continuity
decision first: a regenerated effect is a slightly different effect, and
the duck has to be the same duck next week.

**ffmpeg ships in the image.** `internal/mix` shells out to it — the only
subprocess in the server. It is a hardened static PIE binary copied in
from `mwader/static-ffmpeg`, so the base image stays
`distroless/static-debian12:nonroot`, with no libc, no shell, no package
manager, and `CGO_ENABLED=0` intact.

The cost is **140MB**, taking the image from ~38MB to ~179MB. That is a
stock build carrying every codec its publisher enables — x264, x265, AV1,
SVT-AV1, and a long tail of video work this server will never do, to get
one audio filter graph and an MP3 encoder. Compiling a trimmed ffmpeg
(`--disable-everything` plus mp3, `amix`, `concat`, `aresample`, `volume`)
would land closer to 10–15MB at the price of a slow build stage. Taken as
stock for now on the grounds that Cloud Run streams and caches layers and
this is a cold-start cost, not a per-request one — but it is a bigger
number than the decision was originally made against, and worth revisiting
if starts get slow.

**The credit leads.** For this program the station credit is spoken first,
in the narrator's voice, so the episode ends on the story and silence.
Attribution is unchanged; only its position is.

## Considered options

**Widen `submit_episode` and reuse the storyteller.** Rejected: the two
deliverables share only title and summary, and the old agent must keep
seeing the schema it was versioned against.

**Graceful degradation to single-voice.** Rejected above: the degraded
output is the defect being fixed.

**Rewrite `Engine` as `Render(segments)` across all engines.** Rejected
for blast radius — it changes news and ambient to serve a program neither
of them has.

**Inline markup in prose (`<es voice=tutor>vacío</es>`).** Rejected:
parsing model-authored markup is fragile, and the model will invent tags.

**Pure-Go mixing** (decode, mix, re-encode with a Shine encoder).
Rejected: noticeably worse than LAME, and every episode would pass
through it.

**A per-user character/voice library.** Rejected, consistent with ADR
0011: the cast lives on the canonical Episode.

## Consequences

The chooser can now show a program that this deployment cannot run, so
`AvailableTemplates` gained a second reason to hide one. A Generation may
be resumed on an instance configured differently from the one that took
it, so `performAndPublish` re-checks its dependencies rather than trusting
the chooser.

Cost per episode rises sharply against edge-tts: `eleven_v3` dialogue,
generated effects, and a composed bed. `DialogueRequests`, `SFXGenerated`
and `SFXCacheHits` join the meters so this is visible early. As ever these
are raw counts; dollars come from the vendors, never from a price table
here.

`StripHeaders` becomes a no-op on this path, since ffmpeg emits one clean
file. The call stays, harmlessly, because it is already non-fatal.

**Levels and rhythm are deliberately not solved.** There is no loudness
normalisation, no ducking of the bed under speech, no fade-out, and the
agent is not directed to place pauses for pacing. The bed sits at a fixed
−18dB and everything else arrives at whatever level the vendor returned.
This was raised and consciously deferred. It is the known gap: a generated
effect that comes back hot will be loud in a story meant for a sleeping
child, and the honest way to find that is to listen to real episodes. The
`pause` segment kind exists in the schema against that work, and currently
renders nothing.

> Superseded by ADR 0033. Levels are now measured relative to the
> narration, the bed fades and outlasts the last word, and pauses render.
> Ducking is still not done, and is still a deliberate deferral. The
> filter-surface estimate above is also out of date — see ADR 0033.

Seam quality depends on that same gap. Dialogue prosody does not carry
across a request boundary, and the packer prefers to break where a sound
effect covers the seam — but with no pause segments in play, a quiet story
has fewer places to hide one and will break mid-conversation.

Filling in the voice roster is listening work, not programming work. Until
somebody auditions and picks real voices per role per language, several
roles share a voice and a duck can sound like the narrator.

## Addendum, 2026-08-04: the reading is retired and the name is plain

The two storytelling programs ran side by side for two days, which was
long enough to answer the question the split was hedging against: the
performed stories are the ones worth keeping. The plain reading is gone —
its registry entry, its system prompt, and its `podcasting-storyteller`
agent are no longer pushed — and the performed program takes the name and
the URL that were its: **Story Time**, at `stories`.

What this costs is the fallback. An instance without an ElevenLabs key,
sound effects and ffmpeg now offers no storytelling program at all, where
before it offered the free single-voice one. That is the same trade this
ADR already made for the program itself, taken one step further: a flat
reading of a bilingual bedtime story is not a lesser version of this
program, it is the bug, and keeping it on the chooser only made the bug
easier to reach.

The stored id `stories-v2` outlives the rename. Generations, Episodes and
Beats carry it, so `TemplateByID` resolves it to `stories` rather than
rewriting records in the store. The agent keeps its `-v2` name for the
same reason: the platform treats the name as a version lineage, and
renaming it would restart that lineage on top of the retired
storyteller's.

One thing the retirement broke and this addendum fixes: character
backfill read `Generation.Script` as prose only. With every cast-carrying
program now performed, that path found nothing to extract, so it reads
either shape (`StoredScriptText`).
