package store

import (
	"testing"
	"time"
)

var beatNow = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

func TestBeatDue(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name string
		beat Beat
		want bool
	}{
		{
			name: "a fresh beat is not due until its first interval passes",
			beat: Beat{IntervalDays: 1, CreatedAt: beatNow.Add(-2 * time.Hour)},
			want: false,
		},
		{
			// The form that created it already produced the first Episode,
			// so the clock is anchored to creation even before any firing.
			name: "never fired, but a whole interval since creation",
			beat: Beat{IntervalDays: 1, CreatedAt: beatNow.Add(-25 * time.Hour)},
			want: true,
		},
		{
			name: "exactly on the boundary counts as due",
			beat: Beat{IntervalDays: 7, CreatedAt: beatNow.Add(-30 * day), LastFiredAt: beatNow.Add(-7 * day)},
			want: true,
		},
		{
			name: "a firing resets the clock",
			beat: Beat{IntervalDays: 7, CreatedAt: beatNow.Add(-30 * day), LastFiredAt: beatNow.Add(-1 * day)},
			want: false,
		},
		{
			name: "paused is never due, however overdue",
			beat: Beat{IntervalDays: 1, Paused: true, CreatedAt: beatNow.Add(-90 * day)},
			want: false,
		},
		{
			// Defensive: a zero interval would otherwise be due forever,
			// firing on every single request the owner makes.
			name: "a zero interval never fires",
			beat: Beat{IntervalDays: 0, CreatedAt: beatNow.Add(-90 * day)},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.beat.Due(beatNow); got != tc.want {
				t.Errorf("Due = %v, want %v (due at %s)", got, tc.want, tc.beat.DueAt())
			}
		})
	}
}

func TestBeatGapDays(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name string
		beat Beat
		want int
	}{
		{
			name: "on cadence: the window is the cadence",
			beat: Beat{IntervalDays: 1, LastSucceededAt: beatNow.Add(-1 * day)},
			want: 1,
		},
		{
			// The whole point: a listener back after ten quiet days gets one
			// Episode covering the ten days, not one covering today.
			name: "a long silence stretches the window to the gap",
			beat: Beat{IntervalDays: 1, LastSucceededAt: beatNow.Add(-10 * day)},
			want: 10,
		},
		{
			name: "the window never narrows below the cadence",
			beat: Beat{IntervalDays: 7, LastSucceededAt: beatNow.Add(-2 * day)},
			want: 7,
		},
		{
			name: "capped at the widest window the form offers",
			beat: Beat{IntervalDays: 1, LastSucceededAt: beatNow.Add(-900 * day)},
			want: MaxBeatGapDays,
		},
		{
			// Never succeeded — the anchor is creation, which is when the
			// form's own first Episode covered the ground.
			name: "no success yet falls back to creation",
			beat: Beat{IntervalDays: 1, CreatedAt: beatNow.Add(-4 * day)},
			want: 4,
		},
		{
			// A failing Beat keeps firing on cadence, but each attempt still
			// asks for everything since the last Episode that actually
			// landed — the failures must not silently drop those days.
			name: "failures do not move the anchor",
			beat: Beat{
				IntervalDays:    1,
				LastSucceededAt: beatNow.Add(-3 * day),
				LastFiredAt:     beatNow.Add(-1 * day),
			},
			want: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.beat.GapDays(beatNow); got != tc.want {
				t.Errorf("GapDays = %d, want %d", got, tc.want)
			}
		})
	}
}
