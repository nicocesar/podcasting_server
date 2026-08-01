package store

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func ny(t *testing.T) *time.Location {
	t.Helper()
	loc, err := LoadZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// at builds a wall time in loc, which is how an Anchor is always meant.
func at(loc *time.Location, y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, loc)
}

// TestAnchorDoesNotDrift is the whole point of measuring from the Anchor
// rather than from the firing. A Tick catches a 07:00 Beat at 07:13; if
// tomorrow were measured from 07:13 the Beat would ratchet forward by up
// to a Tick every day and wander through the clock over a month.
func TestAnchorDoesNotDrift(t *testing.T) {
	loc := ny(t)
	b := Beat{
		IntervalDays: 1,
		FireAt:       "07:00",
		AnchorAt:     at(loc, 2026, 7, 1, 7, 0),
		LastFiredAt:  at(loc, 2026, 7, 1, 7, 13), // fired 13 minutes late
	}

	// Ten days of firing late, each time recording the Slot it fired for.
	for day := 2; day <= 12; day++ {
		now := at(loc, 2026, 7, day, 7, 13)
		slot := b.Slot(now, loc)
		want := at(loc, 2026, 7, day, 7, 0)
		if !slot.Equal(want) {
			t.Fatalf("day %d: slot = %s, want %s", day, slot.In(loc), want)
		}
		b.AnchorAt = slot
		b.LastFiredAt = now
	}

	// After ten days the Anchor is still seven in the morning.
	if got := b.AnchorAt.In(loc).Format("15:04"); got != "07:00" {
		t.Errorf("after ten late firings the anchor is %s, want 07:00", got)
	}
}

// TestLooseBeatWouldHaveDrifted pins the contrast: with no FireAt there is
// no wall clock to hold, so the Beat keeps whatever hour its anchor had.
// It still does not ratchet, because the next occurrence is measured from
// the anchor and not from the late firing.
func TestLooseBeatKeepsItsAnchorHour(t *testing.T) {
	loc := ny(t)
	b := Beat{
		IntervalDays: 1,
		AnchorAt:     at(loc, 2026, 7, 1, 14, 30),
	}
	for day := 2; day <= 6; day++ {
		now := at(loc, 2026, 7, day, 14, 47) // noticed 17 minutes late
		slot := b.Slot(now, loc)
		want := at(loc, 2026, 7, day, 14, 30)
		if !slot.Equal(want) {
			t.Fatalf("day %d: slot = %s, want %s", day, slot.In(loc), want)
		}
		b.AnchorAt = slot
	}
}

// TestAnchorSurvivesDaylightSaving: seven in the morning stays seven in
// the morning across both transitions. The UTC instant moves by an hour;
// the wall clock, which is what "my morning briefing" means, does not.
func TestAnchorSurvivesDaylightSaving(t *testing.T) {
	loc := ny(t)
	cases := []struct {
		name          string
		anchor, wants time.Time
	}{
		{
			// 2026 springs forward on Sunday 8 March.
			name:   "spring forward",
			anchor: at(loc, 2026, 3, 7, 7, 0),
			wants:  at(loc, 2026, 3, 8, 7, 0),
		},
		{
			// 2026 falls back on Sunday 1 November.
			name:   "fall back",
			anchor: at(loc, 2026, 10, 31, 7, 0),
			wants:  at(loc, 2026, 11, 1, 7, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := Beat{IntervalDays: 1, FireAt: "07:00", AnchorAt: tc.anchor}
			got := b.DueAt(loc)
			if !got.Equal(tc.wants) {
				t.Fatalf("next = %s, want %s", got.In(loc), tc.wants.In(loc))
			}
			if w := got.In(loc).Format("15:04"); w != "07:00" {
				t.Errorf("wall clock drifted to %s across the transition", w)
			}
			// The instants really are 23 or 25 hours apart, which is the
			// thing adding 24 hours would have got wrong.
			gap := got.Sub(tc.anchor)
			if tc.name == "spring forward" && gap != 23*time.Hour {
				t.Errorf("gap = %s, want 23h", gap)
			}
			if tc.name == "fall back" && gap != 25*time.Hour {
				t.Errorf("gap = %s, want 25h", gap)
			}
		})
	}
}

// TestWeeklyAnchorKeepsItsWeekday: no day-of-week field is stored, so the
// weekday has to fall out of the arithmetic. It does, because seven
// calendar days from a Tuesday is always a Tuesday.
func TestWeeklyAnchorKeepsItsWeekday(t *testing.T) {
	loc := ny(t)
	b := Beat{IntervalDays: 7, FireAt: "07:00", AnchorAt: at(loc, 2026, 3, 3, 7, 0)}
	if b.AnchorAt.In(loc).Weekday() != time.Tuesday {
		t.Fatal("fixture is not a Tuesday")
	}
	// Across the March transition and on for two months.
	for range 9 {
		next := b.DueAt(loc)
		if d := next.In(loc).Weekday(); d != time.Tuesday {
			t.Fatalf("drifted to %s at %s", d, next.In(loc))
		}
		if w := next.In(loc).Format("15:04"); w != "07:00" {
			t.Fatalf("drifted to %s at %s", w, next.In(loc))
		}
		b.AnchorAt = next
	}
}

// TestGraceWindow: an Anchored Beat may be late, but not so late that a
// morning briefing arrives after dark.
func TestGraceWindow(t *testing.T) {
	loc := ny(t)
	anchor := at(loc, 2026, 7, 1, 7, 0)
	b := Beat{IntervalDays: 1, FireAt: "07:00", AnchorAt: anchor}

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before the anchor", at(loc, 2026, 7, 2, 6, 59), false},
		{"on the anchor", at(loc, 2026, 7, 2, 7, 0), true},
		{"one tick late", at(loc, 2026, 7, 2, 7, 15), true},
		{"at the edge of grace", anchor.AddDate(0, 0, 1).Add(BeatGrace), true},
		{"a minute past grace", anchor.AddDate(0, 0, 1).Add(BeatGrace + time.Minute), false},
		{"that evening", at(loc, 2026, 7, 2, 21, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.Due(tc.now, loc); got != tc.want {
				t.Errorf("Due(%s) = %v, want %v", tc.now.In(loc), got, tc.want)
			}
		})
	}
}

// TestSkippedAnchorComesRoundTomorrow: missing a morning must not mean
// missing every morning after it. The Beat is not due that evening, and
// is due again at the next anchor — with no write in between, because
// the Slot is recomputed rather than stored.
func TestSkippedAnchorComesRoundTomorrow(t *testing.T) {
	loc := ny(t)
	b := Beat{IntervalDays: 1, FireAt: "07:00", AnchorAt: at(loc, 2026, 7, 1, 7, 0)}

	evening := at(loc, 2026, 7, 2, 21, 0)
	if b.Due(evening, loc) {
		t.Fatal("fired in the evening")
	}
	tomorrow := at(loc, 2026, 7, 3, 7, 5)
	if !b.Due(tomorrow, loc) {
		t.Fatal("a skipped morning killed every morning after it")
	}
	if slot := b.Slot(tomorrow, loc); !slot.Equal(at(loc, 2026, 7, 3, 7, 0)) {
		t.Errorf("slot = %s, want the 3rd at 07:00 — not the morning we skipped", slot.In(loc))
	}
}

// TestLooseBeatIgnoresGrace: with no time of day there is nothing to be
// late for, so a loose Beat still fires whenever a Tick notices it. This
// is the behaviour every Beat had before Anchors, and it must survive.
func TestLooseBeatIgnoresGrace(t *testing.T) {
	loc := ny(t)
	b := Beat{IntervalDays: 1, AnchorAt: at(loc, 2026, 7, 1, 7, 0)}
	if !b.Due(at(loc, 2026, 7, 2, 23, 0), loc) {
		t.Error("a loose beat was held to a grace window it never asked for")
	}
}

// TestDormantOwnerReturns: away for a month, the Beat fires once for the
// morning it comes back to, not once for every morning it missed.
func TestDormantOwnerReturns(t *testing.T) {
	loc := ny(t)
	b := Beat{
		IntervalDays: 1, FireAt: "07:00",
		AnchorAt: at(loc, 2026, 6, 1, 7, 0),
		// The gap rule measures from the last Episode that landed, not
		// from the last attempt, so the fixture needs both.
		LastSucceededAt: at(loc, 2026, 6, 1, 7, 0),
	}

	now := at(loc, 2026, 7, 1, 7, 30)
	slot := b.Slot(now, loc)
	if !slot.Equal(at(loc, 2026, 7, 1, 7, 0)) {
		t.Fatalf("slot = %s, want this morning", slot.In(loc))
	}
	if !b.Due(now, loc) {
		t.Error("not due on the morning the owner came back")
	}
	// The window covers the whole absence (ADR 0016's gap rule).
	if got := b.GapDays(now); got != 30 {
		t.Errorf("GapDays = %d, want 30", got)
	}
}

// TestAnchorMigration: a Beat written before Anchors existed has no
// AnchorAt, and must inherit the instant it last actually fired rather
// than being treated as never-fired. This is the whole migration — no
// backfill, no write.
func TestAnchorMigration(t *testing.T) {
	loc := ny(t)
	fired := at(loc, 2026, 7, 1, 14, 30)
	b := Beat{
		IntervalDays: 1,
		CreatedAt:    at(loc, 2026, 1, 1, 9, 0),
		LastFiredAt:  fired,
		// AnchorAt deliberately zero.
	}
	if got := b.DueAt(loc); !got.Equal(at(loc, 2026, 7, 2, 14, 30)) {
		t.Errorf("next = %s, want one day after the last firing", got.In(loc))
	}
	// Not due yet: it must not read as never-fired and fire immediately.
	if b.Due(at(loc, 2026, 7, 1, 15, 0), loc) {
		t.Error("a migrated beat fired immediately")
	}
}

// TestAnchorInsideTheSkippedHour documents the one day a year an Anchor
// between 02:00 and 03:00 is not honoured. Go resolves a wall time that
// does not exist backward into the old offset, so it lands an hour early
// rather than an hour late. Asserted so a Go change would be noticed.
func TestAnchorInsideTheSkippedHour(t *testing.T) {
	loc := ny(t)
	b := Beat{IntervalDays: 1, FireAt: "02:30", AnchorAt: at(loc, 2026, 3, 7, 2, 30)}

	next := b.DueAt(loc)
	if d := next.In(loc).Day(); d != 8 {
		t.Fatalf("landed on the %d, want the 8th", d)
	}
	if got := next.In(loc).Format("15:04"); got != "01:30" {
		t.Errorf("skipped-hour anchor resolved to %s, want 01:30 (an hour early, once a year)", got)
	}
	// And it is back to normal the next day.
	b.AnchorAt = next
	if got := b.DueAt(loc).In(loc).Format("15:04"); got != "02:30" {
		t.Errorf("the day after, anchor is %s, want 02:30", got)
	}
}

// TestZoneChangesTheInstant: the same Anchor in two zones is two
// different moments, which is the whole reason a Home Zone is stored.
func TestZoneChangesTheInstant(t *testing.T) {
	nyLoc := ny(t)
	tokyo, err := LoadZone("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	b := Beat{IntervalDays: 1, FireAt: "07:00", AnchorAt: base}

	inNY, inTokyo := b.DueAt(nyLoc), b.DueAt(tokyo)
	if inNY.Equal(inTokyo) {
		t.Fatal("the zone made no difference")
	}
	if got := inNY.In(nyLoc).Format("15:04"); got != "07:00" {
		t.Errorf("New York: %s", got)
	}
	if got := inTokyo.In(tokyo).Format("15:04"); got != "07:00" {
		t.Errorf("Tokyo: %s", got)
	}
}

func TestParseAndValidateFireAt(t *testing.T) {
	good := []string{"00:00", "07:00", "07:05", "23:59", "12:30"}
	for _, s := range good {
		if _, _, ok := ParseFireAt(s); !ok {
			t.Errorf("ParseFireAt(%q) rejected a valid time", s)
		}
		if err := ValidateFireAt(s); err != nil {
			t.Errorf("ValidateFireAt(%q) = %v", s, err)
		}
	}
	bad := []string{"7:00", "24:00", "23:60", "07", "0700", "07:00:00", "-1:00", "aa:bb", " 07:00"}
	for _, s := range bad {
		if _, _, ok := ParseFireAt(s); ok {
			t.Errorf("ParseFireAt(%q) accepted nonsense", s)
		}
		if err := ValidateFireAt(s); err == nil {
			t.Errorf("ValidateFireAt(%q) accepted nonsense", s)
		}
	}
	// Empty is not an hour and not an error: it is what a loose Beat has.
	if _, _, ok := ParseFireAt(""); ok {
		t.Error(`ParseFireAt("") reported an hour`)
	}
	if err := ValidateFireAt(""); err != nil {
		t.Errorf("ValidateFireAt(\"\") = %v, want nil (loose)", err)
	}
}

func TestValidateHomeZone(t *testing.T) {
	for _, s := range []string{"America/New_York", "Asia/Tokyo", "UTC", "Europe/London"} {
		if err := ValidateHomeZone(s); err != nil {
			t.Errorf("ValidateHomeZone(%q) = %v", s, err)
		}
	}
	// Empty means unset, which every existing User is.
	if err := ValidateHomeZone(""); err != nil {
		t.Errorf(`ValidateHomeZone("") = %v, want nil`, err)
	}
	// "Local" is the container's zone — UTC on Cloud Run, nobody's morning.
	if err := ValidateHomeZone("Local"); err == nil {
		t.Error(`ValidateHomeZone("Local") was accepted`)
	}
	for _, s := range []string{"Mars/Olympus", "EST5EDT-ish", "-05:00", "New_York"} {
		if err := ValidateHomeZone(s); err == nil {
			t.Errorf("ValidateHomeZone(%q) was accepted", s)
		}
	}
}

// TestZoneDataIsInTheBinary: a Home Zone is useless if the process cannot
// resolve it, and the base image is not this program's business. The
// blank import of time/tzdata in cmd/server is what guarantees this; the
// test package gets it from its own import above.
func TestZoneDataIsAvailable(t *testing.T) {
	for _, name := range []string{"America/New_York", "Asia/Tokyo", "Europe/Madrid", "Australia/Sydney"} {
		if _, err := LoadZone(name); err != nil {
			t.Errorf("LoadZone(%q): %v — is time/tzdata imported?", name, err)
		}
	}
}
