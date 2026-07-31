package generation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// brokenBeats fails ListBeats for one user and behaves normally for every
// other call, so a pass can be made to survive a failure partway through.
type brokenBeats struct {
	store.Store
	failFor string
}

func (b brokenBeats) ListBeats(ctx context.Context, userID string) ([]store.Beat, error) {
	if userID == b.failFor {
		return nil, errors.New("beats unreadable")
	}
	return b.Store.ListBeats(ctx, userID)
}

// dueBeat provisions a user, marks them live, and gives them one Beat
// that is already overdue.
func dueBeat(t *testing.T, st store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: userID, Title: userID}); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchUser(ctx, userID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-72 * time.Hour)
	if err := st.PutBeat(ctx, store.Beat{
		UserID: userID, ID: "beat-" + userID,
		Template: "news", Topic: "the news", LengthMinutes: 3,
		FreshnessDays: 1, Language: "en", IntervalDays: 1,
		LastFiredAt: past, LastSucceededAt: past, CreatedAt: past,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTickReportsSuccessAfterAFiringError is the invariant standing
// between a bad hour and a duplicated TTS bill: Cloud Scheduler retries a
// non-2xx, and a retry after firings re-fires them. So once anything has
// been fired, a pass must answer success and carry the failure in its
// status instead.
func TestTickReportsSuccessAfterAFiringError(t *testing.T) {
	base := testStore(t)
	st := brokenBeats{Store: base, failFor: "bob"}
	r := testRunner(st, newFakeAPI(), fakeEngine{name: "edge-tts"})
	ctx := context.Background()

	// alice sorts before bob by liveness, so she fires before the failure.
	dueBeat(t, st, "alice")
	time.Sleep(2 * time.Millisecond)
	dueBeat(t, st, "bob")
	time.Sleep(2 * time.Millisecond)
	dueBeat(t, st, "carol")

	status, err := r.Tick(ctx, TickOptions{})
	if err != nil {
		t.Fatalf("a pass that fired something returned an error: %v", err)
	}
	if status.BeatsFired != 2 {
		t.Errorf("BeatsFired = %d, want 2 — one user's bad day cost the others theirs",
			status.BeatsFired)
	}
	if status.Error == "" {
		t.Error("the failure vanished: a pass reports success by design, so " +
			"without Error on the status nothing anywhere records it")
	}
}

// TestTickFailsBeforeAnyFiring: the one path that legitimately answers
// non-2xx. Nothing has been fired, so the retry it provokes is free.
type brokenLiveness struct{ store.Store }

func (brokenLiveness) ListUsersSeenSince(context.Context, time.Time, int) ([]store.User, error) {
	return nil, errors.New("datastore unavailable")
}

func TestTickFailsBeforeAnyFiring(t *testing.T) {
	r := testRunner(brokenLiveness{testStore(t)}, newFakeAPI(), fakeEngine{name: "edge-tts"})

	status, err := r.Tick(context.Background(), TickOptions{})
	if err == nil {
		t.Fatal("a pass that could not read the liveness gate reported success")
	}
	if status.BeatsFired != 0 {
		t.Errorf("BeatsFired = %d, want 0 — a retry would double-fire these", status.BeatsFired)
	}
}

// TestTickDefaults: a zero TickOptions must take the documented defaults
// rather than meaning "window of nothing, budget of nothing".
func TestTickDefaults(t *testing.T) {
	got := TickOptions{}.withDefaults()
	if got.LivenessWindow != DefaultLivenessWindow {
		t.Errorf("LivenessWindow = %v, want %v", got.LivenessWindow, DefaultLivenessWindow)
	}
	if got.BeatBudget != DefaultBeatBudget {
		t.Errorf("BeatBudget = %d, want %d", got.BeatBudget, DefaultBeatBudget)
	}
	if got.UserScanLimit != DefaultUserScanLimit {
		t.Errorf("UserScanLimit = %d, want %d", got.UserScanLimit, DefaultUserScanLimit)
	}
	if got.Trigger != TriggerScheduler {
		t.Errorf("Trigger = %q, want %q", got.Trigger, TriggerScheduler)
	}
}
