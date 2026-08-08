package tts

import (
	"strings"
	"testing"
)

// The bug this whole design exists to fix: a girl and a boy in the same
// story, both `child`, both cast as Wilf.
func TestTwoChildrenGetDifferentVoices(t *testing.T) {
	c, err := CastStory([]Member{
		{ID: "narrator", Name: "Narrator", Role: "narrator", Register: "female"},
		{ID: "emily", Name: "Emily", Role: "child", Register: "female"},
		{ID: "tom", Name: "Tom", Role: "child", Register: "male"},
	}, "en", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	emily, _ := c.Voice("emily")
	tom, _ := c.Voice("tom")
	narrator, _ := c.Voice("narrator")

	if emily.Eleven == tom.Eleven {
		t.Error("two children share a voice")
	}
	if emily.Gender != "female" {
		t.Errorf("Emily was cast %q, want a female voice (this is the original bug)", emily.Gender)
	}
	if tom.Gender != "male" {
		t.Errorf("Tom was cast %q, want a male voice", tom.Gender)
	}
	if emily.Eleven == narrator.Eleven || tom.Eleven == narrator.Eleven {
		t.Error("a child drew the narrator's voice; the narrator is supposed to be reserved")
	}
	if c.Distinct() != 3 {
		t.Errorf("got %d distinct voices for 3 cast members", c.Distinct())
	}
}

// Same name, same voice, with nothing stored — this is what makes a
// returning character sound like themselves before the record exists.
func TestCastingIsStableAcrossStories(t *testing.T) {
	cast := func(extra ...Member) Casting {
		t.Helper()
		members := append([]Member{
			{ID: "narrator", Name: "Narrator", Role: "narrator", Register: "female"},
			{ID: "emily", Name: "Emily", Role: "child", Register: "female"},
		}, extra...)
		c, err := CastStory(members, "en", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, _ := cast().Voice("emily")
	again, _ := cast().Voice("emily")
	if first.Eleven != again.Eleven {
		t.Errorf("Emily was recast between stories: %q then %q", first.ElevenName, again.ElevenName)
	}
	// A different name is a different person and may draw anything; what
	// matters is that adding one does not move Emily.
	withFriend, _ := cast(Member{ID: "priya", Name: "Priya", Role: "child", Register: "female"}).Voice("emily")
	if withFriend.Eleven != first.Eleven {
		t.Errorf("Emily changed voice when another girl joined the story: %q then %q",
			first.ElevenName, withFriend.ElevenName)
	}
}

// Case and stray spacing are not part of who somebody is.
func TestCastingIgnoresNameFormatting(t *testing.T) {
	one, err := CastStory([]Member{{ID: "e", Name: "Emily", Role: "child", Register: "female"}}, "en", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := CastStory([]Member{{ID: "e", Name: "  emily ", Role: "child", Register: "female"}}, "en", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := one.Voice("e")
	b, _ := two.Voice("e")
	if a.Eleven != b.Eleven {
		t.Errorf("%q and %q were cast differently", "Emily", "  emily ")
	}
}

// A recorded voice outranks the hash, so widening the pool cannot recast
// a character who already exists in somebody's feed.
func TestReturningCastKeepsItsRecordedVoice(t *testing.T) {
	// Any curated English child voice that the hash would not pick.
	pool := candidates("en", "child", "female")
	if len(pool) < 2 {
		t.Skip("needs at least two female child voices to be meaningful")
	}
	fresh, err := CastStory([]Member{{ID: "emily", Name: "Emily", Role: "child", Register: "female"}}, "en", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	hashed, _ := fresh.Voice("emily")

	var other storyVoice
	for _, sv := range pool {
		if sv.Eleven != hashed.Eleven {
			other = sv
			break
		}
	}
	c, err := CastStory([]Member{{ID: "emily", Name: "Emily", Role: "child", Register: "female"}},
		"en", "", map[string]string{"emily": other.Eleven})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.Voice("emily")
	if got.Eleven != other.Eleven {
		t.Errorf("recorded voice %q was ignored in favour of %q", other.Name, got.ElevenName)
	}
	if len(c.Notices) != 0 {
		t.Errorf("a clean returning cast should be quiet, got %v", c.Notices)
	}
}

// A voice retired from the table degrades visibly rather than silently.
func TestRetiredRecordedVoiceIsRecastWithANotice(t *testing.T) {
	c, err := CastStory([]Member{{ID: "emily", Name: "Emily", Role: "child", Register: "female"}},
		"en", "", map[string]string{"emily": "a-voice-we-no-longer-cast"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := c.Voice("emily")
	if v.Eleven == "" || v.Eleven == "a-voice-we-no-longer-cast" {
		t.Errorf("want a fresh curated voice, got %q", v.Eleven)
	}
	if len(c.Notices) == 0 {
		t.Error("recasting a retired voice should leave a notice")
	}
}

// More girls than female child voices: borrow a neighbouring part before
// ever repeating, and say so.
func TestExhaustedPoolWidensBeforeItRepeats(t *testing.T) {
	pool := candidates("en", "child", "female")
	members := []Member{{ID: "narrator", Name: "Narrator", Role: "narrator", Register: "female"}}
	for i := range len(pool) + 1 {
		members = append(members, Member{
			ID:       string(rune('a' + i)),
			Name:     "Girl" + string(rune('A'+i)),
			Role:     "child",
			Register: "female",
		})
	}
	c, err := CastStory(members, "en", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Distinct() != len(members) {
		t.Errorf("got %d distinct voices for %d cast members: somebody was repeated",
			c.Distinct(), len(members))
	}
	if len(c.Notices) == 0 {
		t.Error("borrowing a voice from another part should leave a notice")
	}
	if !strings.Contains(strings.Join(c.Notices, "; "), "borrowed") {
		t.Errorf("notice should say a voice was borrowed, got %v", c.Notices)
	}
}

// An episode already written and paid for is not lost over a thin pool.
// Every member still gets a usable voice, however many there are.
func TestAnOverstuffedCastStillCastsEverybody(t *testing.T) {
	var members []Member
	for i := range MaxDialogueVoices * 2 {
		members = append(members, Member{
			ID:       string(rune('a' + i)),
			Name:     "Mouse" + string(rune('A'+i)),
			Role:     "small_squeaky",
			Register: "female",
		})
	}
	c, err := CastStory(members, "en", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		v, ok := c.Voice(m.ID)
		if !ok || v.Eleven == "" {
			t.Fatalf("%s was left without a voice", m.Name)
		}
	}
	if len(c.Notices) == 0 {
		t.Error("a cast this size cannot be clean; it should say so")
	}
}

// The register asked for is honoured wherever the pool can honour it,
// including for the parts that are animals rather than people.
func TestRegisterIsHonouredPerCastMember(t *testing.T) {
	c, err := CastStory([]Member{
		{ID: "narrator", Name: "Narrator", Role: "narrator", Register: "female"},
		{ID: "duck", Name: "Scruff", Role: "small_squeaky", Register: "male"},
		{ID: "mouse", Name: "Nibbles", Role: "small_squeaky", Register: "female"},
	}, "en", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	duck, _ := c.Voice("duck")
	mouse, _ := c.Voice("mouse")
	// A pool entry with no register of its own may fill either, so the
	// assertion is "not the wrong one" rather than "exactly this one".
	if duck.Gender == "female" {
		t.Error("the male-register duck was cast female")
	}
	if mouse.Gender == "male" {
		t.Error("the female-register mouse was cast male")
	}
	if duck.Eleven == mouse.Eleven {
		t.Error("two creatures in one story share a voice")
	}
}

func TestCastStoryNeedsACast(t *testing.T) {
	if _, err := CastStory(nil, "en", "", nil); err == nil {
		t.Fatal("want an error for an empty cast")
	}
}

// Every language the station offers must be able to cast a whole story,
// not just resolve one role at a time — the sibling of
// TestCuratedLanguagesCastEveryRole, one level up.
func TestEveryOfferedLanguageCanCastAStory(t *testing.T) {
	for _, l := range Languages() {
		t.Run(l.Language, func(t *testing.T) {
			c, err := CastStory([]Member{
				{ID: "narrator", Name: "Narrator", Role: "narrator", Register: "female"},
				{ID: "child", Name: "Emily", Role: "child", Register: "female"},
				{ID: "duck", Name: "Scruff", Role: "small_squeaky", Register: "male"},
				{ID: "bear", Name: "Bruno", Role: "big_gruff", Register: "male"},
			}, l.Language, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"narrator", "child", "duck", "bear"} {
				if v, ok := c.Voice(id); !ok || v.Eleven == "" {
					t.Errorf("%s has no voice in %q", id, l.Language)
				}
			}
			// The duck must not sound like the narrator: ADR 0032, held
			// here for every language rather than for English alone.
			narrator, _ := c.Voice("narrator")
			duck, _ := c.Voice("duck")
			if narrator.Eleven == duck.Eleven {
				t.Errorf("the duck sounds like the narrator in %q", l.Language)
			}
		})
	}
}
