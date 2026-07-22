package generation

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// The execution trace answers "what actually happened in this run?" from
// the record itself, instead of from Cloud Logging. It exists because a
// silent TTS fallback — the user asked for one provider, got another —
// left no durable evidence beyond a bumped attempt counter.
//
// One call per event does both jobs: it logs exactly as before, so
// existing `gcloud logging read` habits keep working, and it appends to
// the Generation so an admin can read the same story off the record.
//
// Durability: trace only mutates the in-memory record. Entries reach
// storage on the next PutGeneration, which the runner already performs at
// every stage boundary. Because each stage function returns its
// Generation even on error, and fail() persists that value, entries
// accumulated during a doomed stage are still written exactly once. The
// gap is a hard kill (not a failure) mid-research: those entries are lost
// and the resumed run continues from the stored trace. That is the same
// durability the rest of the checkpoint model offers, so it is left
// alone rather than given a bespoke flush.

// trace records one notable event: logs it, and appends it to g.
// kv is alternating key/value pairs, as in slog; an odd trailing key is
// dropped rather than panicking, because a malformed debug line must
// never take down a generation.
func (r *Runner) trace(g *store.Generation, level, event, msg string, kv ...any) {
	r.traceEntry(g, store.TraceEntry{Level: level, Event: event, Message: msg}, kv...)
}

// traceURL is trace with a deep link attached — the Anthropic Console
// session, which is the single most useful thing an admin can click.
// Explicit rather than a magic key inside kv, which would rot.
func (r *Runner) traceURL(g *store.Generation, level, event, msg, url string, kv ...any) {
	r.traceEntry(g, store.TraceEntry{Level: level, Event: event, Message: msg, URL: url}, kv...)
}

func (r *Runner) traceEntry(g *store.Generation, e store.TraceEntry, kv ...any) {
	kv = kv[:len(kv)-len(kv)%2] // drop an unpaired trailing key

	e.At = time.Now().UTC()
	e.Stage = g.Stage
	e.Detail = traceDetail(kv)
	g.AppendTrace(e)

	// Same fields the runner has always logged, plus the event slug so a
	// log search and the stored trace name things identically.
	args := append([]any{"user", g.UserID, "id", g.ID, "event", e.Event}, kv...)
	if e.URL != "" {
		args = append(args, "trace", e.URL)
	}
	r.log.Log(context.Background(), traceLevel(e.Level), "generation: "+e.Message, args...)
}

// traceLevel maps a trace level onto slog. Notice has no slog equivalent
// — it is Info that an admin should still notice — so it logs at Info and
// is distinguished only on the record.
func traceLevel(level string) slog.Level {
	switch level {
	case store.LevelError:
		return slog.LevelError
	case store.LevelWarn:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// traceDetail renders key/value pairs as a compact JSON object. Errors
// become their message (encoding/json renders a bare error as {}), and
// non-string keys are skipped rather than guessed at.
func traceDetail(kv []any) string {
	if len(kv) == 0 {
		return ""
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		v := kv[i+1]
		if err, ok := v.(error); ok {
			v = err.Error()
		}
		m[k] = v
	}
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
