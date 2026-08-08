# 34. A story declares its cast

Date: 2026-08-08

## Status

Accepted

## Context

A listener asked Story Time for a girl called Emily who makes a friend in
a playground. The episode came back with Emily voiced by a boy, and with
Emily and her new friend voiced by *the same* boy.

Neither was a vendor failure. `internal/tts` cast by **role**, not by
character. A speech segment named one of seven role ids — `narrator`,
`child`, `small_squeaky` and so on — and `RoleVoice(role, language)`
resolved that to exactly one curated voice. Three consequences fell out of
that design and all three were audible in one five-minute story:

**The canon carries no register.** There was nothing in a role, or in a
segment, that could say Emily is a girl. `roleFallback` mapped `child` to
"female" as a last resort, but the English row of `storyVoices` cast
`child` to Wilf, a boy, and the explicit table always won.

**One part, one voice.** `child`, `small_squeaky` and `silly` all resolved
to Wilf in English. Any two children were the same child; the comic part
and the duck were him as well. This was deliberate for "one actor playing
several parts" and it holds up for a duck and a bear — it collapses the
moment two *people* are on stage together.

**Nothing tied a character to a voice across episodes.** The returning
cast picker already existed: a new story can be pointed at a previous
episode's `Characters` and the names come back. The voices never did,
because no voice was ever attached to a name.

ADR 0011 rejected a per-user character library, and that rejection stands
for what it decided — continuity of *description* through Anthropic memory
stores, with the form rendering live platform calls. It did not weigh
voice assignment, which did not exist as a requirement then.

## Decision

**The storyteller declares its cast before it writes the story**, and
segments name a Cast Member rather than a part.

`submit_story` grows a `cast` array: an id, the name the story calls them,
a role from the same closed canon, the vocal register to cast them in, and
a line of description. `segments[].speaker` is now a cast id, validated
against the declared cast. This is a schema change, so the storyteller
agent gets a new version — the same name and the same persona, since it is
still the program that performs rather than reads.

Four decisions inside that:

**Register describes the voice, not the character.** The field is `voice:
female | male`, documented as the vocal register wanted. A girl is female
because she is a girl; a duck is whichever suits a duck. Asking a model
for a tractor's gender is asking the wrong question, and a third bucket
would filter to an empty pool.

**Casting is a hash of the name, probed for collisions.** For each Cast
Member the pool is filtered by `(language, role, register)` and indexed by
an FNV-1a hash of the normalised name, walking forward past any voice
already used in this story. Distinctness is guaranteed within an episode,
and — because the hash is over the name — Emily draws the same voice in
next week's story with nothing stored anywhere. The narrator and the tutor
are exempt: both are pinned and reserved before anyone else draws, the
narrator because it is the station's voice and is named aloud in the
credit, the tutor because being a native speaker of the practiced language
is the entire job.

**A Cast Member keeps one voice in every language they speak.** Casting
per turn by language, which is what the previous design did, made a
bilingual child audibly become a different girl for one word and back
again. The "spoken by a native" guarantee moves to the tutor, who is a
separate Cast Member cast in the target language.

**A thin pool degrades visibly, never fatally.** More girls than female
child voices: borrow a voice of the right register from a neighbouring
role first, then any voice in the language, and only then repeat one —
each with a `LevelNotice` on the trace. An episode that is already written
and paid for is not worth losing over a casting compromise, but an
invisible compromise is the bug ADR 0032 was written against, so the
notice is the part that matters.

Two consequences elsewhere:

**The cast is recorded on the Episode.** `store.Character` grows `Role`,
`Register`, `VoiceID` and `VoiceName`, written from the declared cast at
publish. A recorded voice outranks the hash when that character returns,
so re-curating the pool cannot silently recast somebody who already exists
in a listener's feed.

**The post-publish extraction stops running for stories.** It was a
`claude-haiku-4-5` call per episode that read the finished script to
rediscover a cast the storyteller had just written. It stays in the tree
for the owner-only backfill button, which serves episodes published before
this.

The pool itself was widened by ear before any of this shipped, through a
new `cmd/audition`: it queries the shared library, renders each candidate
through `text-to-dialogue` answering the pinned narrator, and puts them on
one page. English went from five distinct voices to nineteen, including
the female child voices that did not exist at all.

## Consequences

A story with two children sounds like two children. A girl is cast female.
A character who comes back sounds like themselves, first from the hash and
then, once recorded, from the record.

**A stored script written against the old schema no longer parses.** This
is a deliberate hard cutover rather than a compatibility branch: with no
`cast`, the submission is rejected. `Retry` resumes from a stored Script
whenever one is present, so a Generation holding a pre-cutover script
cannot be retried into success and has to be started again. The affected
population was one failed run, and the alternative — teaching `Retry` to
validate its checkpoint — was declined as unnecessary for it. The trap
stays armed for the next schema change, which is the accepted cost.

The pool is only deep in English. Italian and German still cast a role to
one voice, so two children in a German story share an actor and take a
notice for it. `warm_grownup` and `big_gruff` have no female voice in any
language: a mother or a teacher borrows from a neighbouring role. Both are
audition rounds, not code.

Casting is per episode and per name, which means two different characters
who happen to share a name across stories also share a voice. For a
station telling stories to one family, that is a feature.
