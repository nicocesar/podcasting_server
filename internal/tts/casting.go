package tts

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// A story's cast is resolved here: the storyteller declares who is in the
// story, and this file decides which real voice each of them speaks with.
//
// The rule the whole design turns on is that casting is per Cast Member,
// not per Role. Roles are a closed canon of *part types* — the canon is
// what stops the model hallucinating a voice — but two children in one
// story are two people, and a table keyed only by role hands them the
// same actor. That is how a girl called Emily came to be voiced by a boy
// called Wilf: `child` had exactly one English voice, and it was his.

// Member is one Cast Member as the storyteller declared them: somebody in
// the story, not a part in it.
type Member struct {
	ID   string // what segments say in `speaker`
	Name string // what the listener hears them called
	Role string // a part type from the canon, for pool selection
	// Register is the vocal register asked for — a property of the voice
	// wanted, not a fact about the character. A duck has a register.
	Register string
}

// Casting is the finished decision: every Cast Member's voice, plus what
// had to be compromised to get there.
type Casting struct {
	byID     map[string]Voice
	narrator string // cast id of the narrator, for the station credit
	// Notices records every place the cast is not what was asked for — a
	// borrowed voice, a repeat, a recorded voice that no longer exists.
	// Nothing here fails an episode; all of it belongs on the trace,
	// because a degraded cast that nobody can see is the actual bug.
	Notices []string
}

// Voice returns the voice cast for a Cast Member.
func (c Casting) Voice(id string) (Voice, bool) {
	v, ok := c.byID[id]
	return v, ok
}

// Narrator returns the narrator's voice. The credit is spoken in it and
// names it aloud, so it has to be the voice that actually narrated rather
// than whatever the table lists first.
func (c Casting) Narrator() (Voice, bool) {
	if c.narrator == "" {
		return Voice{}, false
	}
	return c.Voice(c.narrator)
}

// Distinct reports how many different voices the casting uses, which is
// what the vendor's per-request ceiling is measured in.
func (c Casting) Distinct() int {
	seen := map[string]bool{}
	for _, v := range c.byID {
		seen[v.Eleven] = true
	}
	return len(seen)
}

// pinnedRoles are cast before anyone else and removed from the pool.
// The narrator is the station's voice — heard most, named in the credit —
// and the tutor is the one part whose whole job is to be a native speaker
// of the language being practiced. Neither should wobble story to story
// on a hash of its own name.
var pinnedRoles = map[string]bool{"narrator": true, "tutor": true}

// CastStory resolves every Cast Member to a voice.
//
// language is the story's base language; target is the language being
// practiced, if any, and is used only for the tutor. returning maps a
// normalised character name to the ElevenLabs id they were cast with last
// time, so a character who comes back sounds like themselves.
//
// The order is deliberate: pinned parts first (so nobody else can draw
// the narrator), then characters with a recorded voice (so continuity
// beats novelty), then everyone else by a hash of their name (so a new
// Emily is the same Emily next week without anything being stored).
func CastStory(members []Member, language, target string, returning map[string]string) (Casting, error) {
	c := Casting{byID: make(map[string]Voice, len(members))}
	if len(members) == 0 {
		return c, fmt.Errorf("cast is empty")
	}
	taken := map[string]bool{}

	// Pinned.
	for _, m := range members {
		if !pinnedRoles[m.Role] {
			continue
		}
		lang := language
		if m.Role == "tutor" && target != "" {
			lang = target
		}
		v, ok := RoleVoice(m.Role, lang)
		if !ok || v.Eleven == "" {
			return c, fmt.Errorf("no %s voice for %q", m.Role, lang)
		}
		c.byID[m.ID] = v
		taken[v.Eleven] = true
		if m.Role == "narrator" && c.narrator == "" {
			c.narrator = m.ID
		}
	}

	// Returning: a recorded voice outranks the hash, so re-curating the
	// pool cannot silently recast a character who already exists in
	// somebody's feed.
	curated := languageVoices(language)
	for _, m := range members {
		if _, done := c.byID[m.ID]; done {
			continue
		}
		id, ok := returning[NormalizeName(m.Name)]
		if !ok || id == "" {
			continue
		}
		sv, known := curated[id]
		if !known {
			c.Notices = append(c.Notices, fmt.Sprintf(
				"%s came back with a voice that is no longer curated; recast", m.Name))
			continue
		}
		if taken[id] {
			c.Notices = append(c.Notices, fmt.Sprintf(
				"%s came back to a voice another character already has; recast", m.Name))
			continue
		}
		c.byID[m.ID] = voiceOf(sv, language)
		taken[id] = true
	}

	// Everyone else.
	for _, m := range members {
		if _, done := c.byID[m.ID]; done {
			continue
		}
		v, notice := pick(m, language, taken)
		if v.Eleven == "" {
			return c, fmt.Errorf("no voice at all for %q in %q", m.Name, language)
		}
		if notice != "" {
			c.Notices = append(c.Notices, notice)
		}
		c.byID[m.ID] = v
		taken[v.Eleven] = true
	}
	return c, nil
}

// pick resolves one Cast Member, widening as it has to. It never fails
// while the language has any curated voice at all: an episode that is
// already written and paid for is not worth losing over a thin pool, and
// the notice is what keeps the compromise visible.
func pick(m Member, language string, taken map[string]bool) (Voice, string) {
	// What was asked for.
	if v, ok := choose(candidates(language, m.Role, m.Register), m.Name, taken); ok {
		return voiceOf(v, language), ""
	}
	// A neighbouring part in the same register: one actor stretching is
	// better than two characters sharing a voice, and eleven_v3 performs
	// the audio tags that make the stretch work.
	if v, ok := choose(candidates(language, "", m.Register), m.Name, taken); ok {
		return voiceOf(v, language), fmt.Sprintf(
			"%s borrowed the %s voice %s: no %s voice was free",
			m.Name, v.Role, v.Name, m.Role)
	}
	// Any voice in the language, still unheard in this story.
	if v, ok := choose(candidates(language, "", ""), m.Name, taken); ok {
		return voiceOf(v, language), fmt.Sprintf(
			"%s borrowed %s, cast against register: no %s voice was free",
			m.Name, v.Name, m.Register)
	}
	// Everything is taken. Repeat somebody rather than lose the episode.
	pool := candidates(language, m.Role, m.Register)
	if len(pool) == 0 {
		pool = candidates(language, "", "")
	}
	if len(pool) == 0 {
		return Voice{}, ""
	}
	v := pool[index(m.Name, len(pool))]
	return voiceOf(v, language), fmt.Sprintf(
		"%s shares %s with another character: the %s pool is exhausted",
		m.Name, v.Name, language)
}

// choose walks the pool from a hash of the name, taking the first voice
// nobody in this story is already using. Starting from the name rather
// than from zero is what makes a character keep their voice across
// episodes with nothing stored: same name, same pool, same answer.
func choose(pool []storyVoice, name string, taken map[string]bool) (storyVoice, bool) {
	if len(pool) == 0 {
		return storyVoice{}, false
	}
	start := index(name, len(pool))
	for i := range pool {
		v := pool[(start+i)%len(pool)]
		if !taken[v.Eleven] {
			return v, true
		}
	}
	return storyVoice{}, false
}

// candidates filters the pool. An empty role or register matches any; a
// pool entry with an empty register reads as neither and can fill either.
func candidates(language, role, register string) []storyVoice {
	var out []storyVoice
	for _, sv := range storyVoices {
		if sv.Language != language {
			continue
		}
		if role != "" && sv.Role != role {
			continue
		}
		if register != "" && sv.Gender != "" && sv.Gender != register {
			continue
		}
		out = append(out, sv)
	}
	return out
}

// languageVoices indexes a language's curated voices by vendor id, for
// checking that a recorded voice is still one we cast.
func languageVoices(language string) map[string]storyVoice {
	out := map[string]storyVoice{}
	for _, sv := range storyVoices {
		if sv.Language == language {
			out[sv.Eleven] = sv
		}
	}
	return out
}

func voiceOf(sv storyVoice, language string) Voice {
	return Voice{Language: language, Gender: sv.Gender, Eleven: sv.Eleven, ElevenName: sv.Name}
}

// index turns a name into a starting position in a pool. FNV-1a because
// it is stable across processes and releases — Go's map hash is seeded
// per process and would recast everybody on every restart.
func index(name string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(NormalizeName(name)))
	return int(h.Sum32() % uint32(n))
}

// NormalizeName is the identity a returning character is matched on, and
// what the hash is taken over. Case and surrounding space are not part of
// who somebody is; "emily" and "Emily " are the same girl. Exported
// because the caller builds its returning-cast index from stored records
// and has to key it exactly the same way.
func NormalizeName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}
