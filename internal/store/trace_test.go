package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func entry(level, event string) TraceEntry {
	return TraceEntry{Level: level, Event: event, Message: event}
}

func TestAppendTraceTruncates(t *testing.T) {
	var g Generation
	g.AppendTrace(TraceEntry{
		Level:   LevelInfo,
		Message: strings.Repeat("m", MaxTraceMessage+50),
		Detail:  strings.Repeat("d", MaxTraceDetail+50),
		URL:     strings.Repeat("u", MaxTraceURL+50),
	})
	got := g.Trace[0]
	if len(got.Message) != MaxTraceMessage {
		t.Errorf("message = %d bytes, want %d", len(got.Message), MaxTraceMessage)
	}
	if len(got.Detail) != MaxTraceDetail {
		t.Errorf("detail = %d bytes, want %d", len(got.Detail), MaxTraceDetail)
	}
	if len(got.URL) != MaxTraceURL {
		t.Errorf("url = %d bytes, want %d", len(got.URL), MaxTraceURL)
	}
}

// TestAppendTraceTruncatesOnRuneBoundary guards against cutting a
// multi-byte rune in half, which would put invalid UTF-8 into Datastore
// and into the admin page. Spanish topics make this reachable, not
// theoretical.
func TestAppendTraceTruncatesOnRuneBoundary(t *testing.T) {
	var g Generation
	// "ñ" is 2 bytes, so a run of them straddles any odd byte limit.
	g.AppendTrace(TraceEntry{Level: LevelInfo, Message: strings.Repeat("ñ", MaxTraceMessage)})
	msg := g.Trace[0].Message
	if !utf8.ValidString(msg) {
		t.Fatalf("truncated message is not valid UTF-8: %q", msg)
	}
	if len(msg) > MaxTraceMessage {
		t.Errorf("message = %d bytes, want <= %d", len(msg), MaxTraceMessage)
	}
}

// TestAppendTraceEvictsInfoFirst is the point of the cap policy: the
// warn/error entries an admin actually needs must survive a flood of
// routine ones.
func TestAppendTraceEvictsInfoFirst(t *testing.T) {
	var g Generation
	g.AppendTrace(entry(LevelWarn, "tts.fallback")) // the one that matters
	for i := 0; i < MaxTraceEntries+20; i++ {
		g.AppendTrace(entry(LevelInfo, "routine"))
	}
	if len(g.Trace) != MaxTraceEntries {
		t.Fatalf("trace = %d entries, want %d", len(g.Trace), MaxTraceEntries)
	}
	if g.Trace[0].Event != "tts.fallback" {
		t.Errorf("the warn entry was evicted; first entry is %q", g.Trace[0].Event)
	}
	if g.TraceDropped != 21 {
		t.Errorf("TraceDropped = %d, want 21", g.TraceDropped)
	}
}

// TestAppendTraceEvictsOldestWhenAllNotable covers the fallback branch:
// with nothing routine left to drop, the cap still holds.
func TestAppendTraceEvictsOldestWhenAllNotable(t *testing.T) {
	var g Generation
	for i := 0; i < MaxTraceEntries; i++ {
		g.AppendTrace(entry(LevelWarn, "first"))
	}
	g.AppendTrace(entry(LevelError, "last"))
	if len(g.Trace) != MaxTraceEntries {
		t.Fatalf("trace = %d entries, want %d", len(g.Trace), MaxTraceEntries)
	}
	if g.Trace[len(g.Trace)-1].Event != "last" {
		t.Error("newest entry was not kept")
	}
	if g.TraceDropped != 1 {
		t.Errorf("TraceDropped = %d, want 1", g.TraceDropped)
	}
}
