package httpapi

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// routeReg matches every route registered on the mux, capturing the
// pattern string: mux.HandleFunc("GET /me/episodes", ...) and
// mux.Handle("GET /static/", ...).
var routeReg = regexp.MustCompile(`mux\.Handle(?:Func)?\("([^"]+)"`)

// topSegment returns the first static path segment of a route pattern —
// "GET /me/episodes/{slug}" -> "me" — or "" when the route has no fixed
// first segment to protect (the bare "/", a "{param}" first segment).
func topSegment(pattern string) string {
	// Drop an optional "METHOD " prefix.
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	pattern = strings.TrimPrefix(pattern, "/")
	seg, _, _ := strings.Cut(pattern, "/")
	if seg == "" || strings.HasPrefix(seg, "{") {
		return ""
	}
	return seg
}

// TestReservedCoversRoutes is the standing guarantee the feature exists
// for: a username can never be minted that collides with a route. It
// reads the router's own source and fails the moment a new endpoint
// introduces a top-level path segment that no account is forbidden to
// take. The fix when it fails is one line — add the segment to
// store.reservedUsernames — which is the entire point: adding a route
// forces the decision.
func TestReservedCoversRoutes(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range routeReg.FindAllStringSubmatch(string(src), -1) {
		seg := topSegment(m[1])
		if seg == "" || seen[seg] {
			continue
		}
		seen[seg] = true
		// Only a segment shaped like a possible username can be shadowed;
		// one with a dot or dash, or the wrong length, could never be
		// registered, so the router need not reserve it.
		if impersonable(seg) && !store.Reserved(seg) {
			t.Errorf("route /%s/… is not a reserved username: a user could register %q and shadow it. Add it to store.reservedUsernames.", seg, seg)
		}
	}
}

// impersonable reports whether a route segment is shaped like something
// a person could actually register as a username, ignoring the reserved
// check itself.
func impersonable(seg string) bool {
	if len(seg) < store.UsernameMinLen || len(seg) > store.UsernameMaxLen {
		return false
	}
	for _, r := range seg {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
