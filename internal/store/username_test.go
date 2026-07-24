package store

import "testing"

func TestValidateUsername(t *testing.T) {
	ok := []string{"alice", "bob99", "a1b2", "twentycharacterlongx"}
	for _, s := range ok {
		if err := ValidateUsername(s); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want nil", s, err)
		}
		// A creatable username must always be a valid ID: it becomes a
		// key, a filename, and a URL segment.
		if !ValidID(s) {
			t.Errorf("ValidateUsername accepts %q but ValidID rejects it", s)
		}
	}

	bad := []struct {
		s, why string
	}{
		{"", "empty"},
		{"abc", "too short (3)"},
		{"twentyonecharacters21", "too long (21)"},
		{"Alice", "uppercase"},
		{"al.ce", "dot"},
		{"al-ce", "dash"},
		{"al_ce", "underscore"},
		{"joe smith", "space"},
		{"admin", "reserved route"},
		{"invites", "reserved route"},
		{"settings", "reserved route"},
		{"root", "reserved role"},
		{"support", "reserved role"},
	}
	for _, c := range bad {
		if err := ValidateUsername(c.s); err == nil {
			t.Errorf("ValidateUsername(%q) = nil, want error (%s)", c.s, c.why)
		}
	}
}

// TestReservedNamesAreThemselvesCreatable guards a subtle trap: a
// reserved word that could never pass the charset or length rules is
// dead weight, and usually a sign the rule and the list disagree. Every
// reserved word should be a name that would otherwise be claimable.
func TestReservedNamesAreThemselvesCreatable(t *testing.T) {
	for w := range reservedUsernames {
		if len(w) < UsernameMinLen || len(w) > UsernameMaxLen {
			// Short router segments like "f" and "me" can never be
			// usernames anyway; reserving them is harmless documentation,
			// not a rule that ever fires. Allowed, not required.
			continue
		}
		if !usernamePattern.MatchString(w) {
			t.Errorf("reserved word %q can never be a username (bad charset); drop it or fix the pattern", w)
		}
	}
}
