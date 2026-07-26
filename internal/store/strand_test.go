package store

import (
	"strings"
	"testing"
	"time"
)

// TestValidateStrandID: a Strand id is a public, permanent URL segment.
// It shares the top-level path namespace with usernames, so the reserved
// list has to apply here too — a strand called "admin" would shadow a
// route, and one called "login" would be worse.
func TestValidateStrandID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{"plain", "music", true},
		{"dashed", "tech-news", true},
		{"digits", "radio4", true},
		{"too short", "ab", false},
		{"too long", strings.Repeat("a", StrandIDMaxLen+1), false},
		{"empty", "", false},
		{"uppercase", "TechNews", false},
		{"underscore", "tech_news", false},
		{"space", "tech news", false},
		{"leading dash", "-music", false},
		{"trailing dash", "music-", false},
		{"double dash", "tech--news", false},
		{"reserved route", "admin", false},
		{"reserved own segment", "strands", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStrandID(tc.id)
			if tc.ok && err != nil {
				t.Fatalf("ValidateStrandID(%q) = %v, want nil", tc.id, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateStrandID(%q) = nil, want an error", tc.id)
			}
		})
	}
}

// TestSeedStrandsAreValid: a fresh install writes these unreviewed, so a
// typo here is a broken canon nobody can fix without a deploy.
func TestSeedStrandsAreValid(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range SeedStrands() {
		if err := ValidateStrand(s); err != nil {
			t.Errorf("seed strand %q: %v", s.ID, err)
		}
		if seen[s.ID] {
			t.Errorf("seed strand %q appears twice", s.ID)
		}
		seen[s.ID] = true
		if !s.Dormant() {
			t.Errorf("seed strand %q is not dormant: it has no cover art yet, so it must not accept airings", s.ID)
		}
	}
}

// TestStrandDormant: a Strand without cover art cannot be aired into,
// because a feed with no <itunes:image> is broken in most clients. A
// retired one cannot either, whatever art it has.
func TestStrandDormant(t *testing.T) {
	tests := []struct {
		name string
		s    Strand
		want bool
	}{
		{"no art", Strand{ID: "music", Title: "Music"}, true},
		{"art", Strand{ID: "music", Title: "Music", CoverType: "image/jpeg"}, false},
		{"retired with art", Strand{ID: "music", Title: "Music", CoverType: "image/jpeg", Retired: true}, true},
		{"retired without art", Strand{ID: "music", Title: "Music", Retired: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Dormant(); got != tc.want {
				t.Fatalf("Dormant() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAiringSettleable: the window closes exactly once, at 24 hours. An
// airing already settled is never settleable again — re-freezing the
// count would let a late vouch change a delivery decision that has
// already been taken.
func TestAiringSettleable(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		a    Airing
		want bool
	}{
		{"fresh", Airing{AiredAt: now.Add(-time.Hour)}, false},
		{"one minute short", Airing{AiredAt: now.Add(-SettleWindow + time.Minute)}, false},
		{"exactly the window", Airing{AiredAt: now.Add(-SettleWindow)}, true},
		{"long past", Airing{AiredAt: now.Add(-90 * 24 * time.Hour)}, true},
		{"already settled", Airing{AiredAt: now.Add(-90 * 24 * time.Hour), Settled: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Settleable(now); got != tc.want {
				t.Fatalf("Settleable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAiringDelivers is the whole of ADR 0019's delivery rule: settled,
// inside the horizon, and at or above the follower's Bar.
func TestAiringDelivers(t *testing.T) {
	now := time.Now().UTC()
	settled := func(vouches int, age time.Duration) Airing {
		return Airing{AiredAt: now.Add(-age), Settled: true, VouchesAtSettle: vouches}
	}
	tests := []struct {
		name string
		a    Airing
		bar  int
		want bool
	}{
		{"unsettled never delivers", Airing{AiredAt: now.Add(-time.Hour), VouchesAtSettle: 9}, 0, false},
		{"firehose takes the unvouched", settled(0, 48*time.Hour), 0, true},
		{"bar of one rejects the unvouched", settled(0, 48*time.Hour), 1, false},
		{"bar of one takes one vouch", settled(1, 48*time.Hour), 1, true},
		{"bar of two rejects one vouch", settled(1, 48*time.Hour), 2, false},
		{"vouches above the bar still deliver", settled(5, 48*time.Hour), 2, true},
		{"inside the horizon", settled(1, DeliveryHorizon-time.Hour), 1, true},
		{"past the horizon", settled(9, DeliveryHorizon+time.Hour), 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Delivers(tc.bar, now); got != tc.want {
				t.Fatalf("Delivers(bar=%d) = %v, want %v", tc.bar, got, tc.want)
			}
		})
	}
}

// TestNewAiringID: the id lands in public URLs and in podcast client
// cache keys, so it has to be url-safe, stable in shape, and not repeat.
func TestNewAiringID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := NewAiringID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("NewAiringID returned %q twice in %d draws", id, i+1)
		}
		seen[id] = true
		if strings.Trim(id, "abcdefghijklmnopqrstuvwxyz234567") != "" {
			t.Fatalf("NewAiringID returned %q, which is not lowercase base32", id)
		}
	}
}

func TestValidateBar(t *testing.T) {
	for _, bar := range []int{0, 1, 2, MaxBar} {
		if err := ValidateBar(bar); err != nil {
			t.Errorf("ValidateBar(%d) = %v, want nil", bar, err)
		}
	}
	for _, bar := range []int{-1, MaxBar + 1} {
		if err := ValidateBar(bar); err == nil {
			t.Errorf("ValidateBar(%d) = nil, want an error", bar)
		}
	}
}
