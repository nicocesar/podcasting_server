package generation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// findTrace returns the first entry with the given event slug.
func findTrace(g store.Generation, event string) (store.TraceEntry, bool) {
	for _, e := range g.Trace {
		if e.Event == event {
			return e, true
		}
	}
	return store.TraceEntry{}, false
}

// traceDetailOf unmarshals an entry's Detail for assertions.
func traceDetailOf(t *testing.T, e store.TraceEntry) map[string]any {
	t.Helper()
	m := map[string]any{}
	if e.Detail == "" {
		return m
	}
	if err := json.Unmarshal([]byte(e.Detail), &m); err != nil {
		t.Fatalf("detail %q is not JSON: %v", e.Detail, err)
	}
	return m
}

// TestTraceRecordsTTSFallback is the incident this whole feature exists
// for: a listener asked for one provider, it failed, and the chain
// quietly used another. Before the trace the only evidence was a bumped
// counter and a log line in Cloud Logging.
func TestTraceRecordsTTSFallback(t *testing.T) {
	st := testStore(t)
	eleven := &recordingEngine{
		name: "elevenlabs",
		err:  fmt.Errorf(`http 402: {"detail":{"code":"paid_plan_required"}}`),
	}
	edge := &recordingEngine{name: "edge-tts"}
	r := testRunner(st, newFakeAPI(), edge, eleven)

	g := newGeneration()
	g.Voice, g.Provider = "female", "elevenlabs"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	fallback, ok := findTrace(g, "tts.fallback")
	if !ok {
		t.Fatalf("no tts.fallback entry; trace = %+v", g.Trace)
	}
	if fallback.Level != store.LevelWarn {
		t.Errorf("fallback level = %q, want warn", fallback.Level)
	}
	d := traceDetailOf(t, fallback)
	if d["engine"] != "elevenlabs" {
		t.Errorf("fallback engine = %v, want elevenlabs", d["engine"])
	}
	// The field that turns "a fallback happened" into "the listener asked
	// for elevenlabs and did not get it".
	if d["requested_provider"] != "elevenlabs" {
		t.Errorf("requested_provider = %v, want elevenlabs", d["requested_provider"])
	}
	if s, _ := d["err"].(string); !strings.Contains(s, "402") {
		t.Errorf("err detail lost the cause: %v", d["err"])
	}

	selected, ok := findTrace(g, "tts.selected")
	if !ok {
		t.Fatal("no tts.selected entry")
	}
	sd := traceDetailOf(t, selected)
	if sd["engine"] != "edge-tts" {
		t.Errorf("selected engine = %v, want edge-tts", sd["engine"])
	}
	if sd["attempts"] != float64(2) {
		t.Errorf("attempts = %v, want 2", sd["attempts"])
	}
}

// TestTracePersistsThroughFailure pins the invariant the whole
// no-extra-writes design rests on: stage functions return their
// Generation even on error, and fail() persists it, so entries recorded
// during a doomed stage still reach storage.
func TestTracePersistsThroughFailure(t *testing.T) {
	st := testStore(t)
	boom := fmt.Errorf("engine exploded")
	r := testRunner(st, newFakeAPI(),
		&recordingEngine{name: "edge-tts", err: boom},
		&recordingEngine{name: "elevenlabs", err: boom})

	g := newGeneration()
	g.Voice = "female"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenFailed)

	if _, ok := findTrace(g, "tts.fallback"); !ok {
		t.Errorf("fallback entries did not survive onto the failure record; trace = %+v", g.Trace)
	}
	failed, ok := findTrace(g, "stage.failed")
	if !ok {
		t.Fatal("no stage.failed entry")
	}
	if failed.Level != store.LevelError {
		t.Errorf("stage.failed level = %q, want error", failed.Level)
	}
}

// TestTraceRecordsSessionAndScript covers the happy path's spine, and
// that the console link is captured as a real URL rather than buried in
// the detail blob.
func TestTraceRecordsSessionAndScript(t *testing.T) {
	st := testStore(t)
	r := testRunner(st, newFakeAPI(), &recordingEngine{name: "edge-tts"})

	g := newGeneration()
	g.Voice = "female"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	created, ok := findTrace(g, "session.created")
	if !ok {
		t.Fatal("no session.created entry")
	}
	if !strings.HasPrefix(created.URL, "https://platform.claude.com/") {
		t.Errorf("session.created URL = %q", created.URL)
	}
	if _, ok := findTrace(g, "script.accepted"); !ok {
		t.Error("no script.accepted entry")
	}
}

// TestTraceRecordsTranslationRound covers the tool-path rejection: the
// agent answered in the wrong language and was asked to translate. Only
// the terminal failure used to survive.
func TestTraceRecordsTranslationRound(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	// English first, Spanish after the rejection.
	api.submissions = []string{scriptInput, spanishInput}
	r := testRunner(st, api, &recordingEngine{name: "edge-tts"})

	g := newGeneration()
	g.Language, g.Voice = "es", "female"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	e, ok := findTrace(g, "script.translation_requested")
	if !ok {
		t.Fatalf("no translation entry; trace = %+v", g.Trace)
	}
	if e.Level != store.LevelNotice {
		t.Errorf("level = %q, want notice", e.Level)
	}
	d := traceDetailOf(t, e)
	if d["got"] != "en" || d["want"] != "es" {
		t.Errorf("detail = %v, want got=en want=es", d)
	}
}

// TestTraceStaysOffTheOwnerRecord guards the json:"-" contract: the
// trace carries raw upstream errors and session ids, and must never ride
// along on the owner-facing poll of /me/generations/{id}.
func TestTraceStaysOffTheOwnerRecord(t *testing.T) {
	var g store.Generation
	g.AppendTrace(store.TraceEntry{
		Level: store.LevelWarn, Event: "tts.fallback",
		Message: "tts engine failed", Detail: `{"err":"secret upstream detail"}`,
	})
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret upstream detail") || strings.Contains(string(b), "tts.fallback") {
		t.Errorf("trace leaked into the owner-facing JSON: %s", b)
	}
}
