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

// TestAiringDelivers: with the Bar and Settling gone (ADR 0027) the
// horizon is the whole delivery rule, so this is the only thing standing
// between a new follower and the entire archive.
func TestAiringDelivers(t *testing.T) {
	now := time.Now().UTC()
	aired := func(age time.Duration) Airing { return Airing{AiredAt: now.Add(-age)} }
	tests := []struct {
		name string
		a    Airing
		want bool
	}{
		{"just aired", aired(time.Minute), true},
		{"yesterday, with no window to wait out", aired(24 * time.Hour), true},
		{"inside the horizon", aired(DeliveryHorizon - time.Hour), true},
		{"exactly the horizon", aired(DeliveryHorizon), true},
		{"past the horizon", aired(DeliveryHorizon + time.Hour), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Delivers(now); got != tc.want {
				t.Fatalf("Delivers() = %v, want %v", got, tc.want)
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
