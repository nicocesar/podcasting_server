package generation

// Stranding: placing a finished Episode in one of the station's Strands
// (ADR 0017). Like character extraction, deliberately not an agent
// session — the script is already written and there is nothing to
// research, so this is one plain schema-constrained /v1/messages call on
// a small model.
//
// The canon travels in the schema as an enum rather than in the prompt
// as a suggestion, so the model cannot coin a Strand by inference. That
// is the whole point of ADR 0017: a fluent tagger inventing `noir`,
// `hardboiled` and `crime-fiction` across three episodes would rebuild
// the free-text fragmentation the closed canon exists to avoid.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const strandModel = "claude-haiku-4-5"

// strandMaxTokens fits one id and a sentence of reasoning. The schema
// does the real constraining.
const strandMaxTokens = 500

// strandScriptRunes caps how much of the script is sent. A sixty-minute
// episode runs to thousands of words, and the subject is settled in the
// first few hundred — paying to classify the rest buys nothing.
const strandScriptRunes = 6000

// StrandNone is the choice the model makes when nothing in the canon
// fits. It is a real answer, not a failure: the pile of Strandless
// Episodes is the evidence for what to add to the canon next, so the
// schema offers it explicitly rather than forcing a bad fit.
const StrandNone = "none"

// StrandRequest is what the classifier reads. Everything is optional
// except Title: an ambient Episode has a Topic and no Script, an
// externally published one has neither.
type StrandRequest struct {
	Title       string
	Description string
	Topic       string
	Template    string
	Script      string
}

// strandSchema builds the output schema for one call. The enum is the
// live canon plus StrandNone, so adding a Strand changes the behaviour
// of every subsequent Stranding with no deploy.
func strandSchema(canon []store.Strand) map[string]any {
	ids := make([]string, 0, len(canon)+1)
	for _, s := range canon {
		ids = append(ids, s.ID)
	}
	ids = append(ids, StrandNone)
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"strand": map[string]any{
				"type":        "string",
				"enum":        ids,
				"description": `The strand this episode belongs in, or "none" if no strand is a good fit.`,
			},
		},
		"required": []string{"strand"},
	}
}

func strandPrompt(canon []store.Strand, req StrandRequest) string {
	var b strings.Builder
	b.WriteString(`You are sorting one episode of an audio station into exactly one of the station's strands.

The strands are:

`)
	for _, s := range canon {
		fmt.Fprintf(&b, "- %s — %s", s.ID, s.Title)
		if s.Description != "" {
			fmt.Fprintf(&b, ": %s", s.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString(`
Choose the one strand this episode belongs in. If none of them is a good
fit, answer "none" — that is a correct and useful answer, and a forced
bad fit is worse than none at all. Judge by what the episode is actually
about, not by which strand is emptiest.

The episode:

`)
	fmt.Fprintf(&b, "Title: %s\n", req.Title)
	if req.Template != "" {
		fmt.Fprintf(&b, "Program: %s\n", req.Template)
	}
	if req.Topic != "" {
		fmt.Fprintf(&b, "Requested subject: %s\n", req.Topic)
	}
	if req.Description != "" {
		fmt.Fprintf(&b, "Summary: %s\n", req.Description)
	}
	if script := truncateRunes(req.Script, strandScriptRunes); script != "" {
		fmt.Fprintf(&b, "\nScript:\n%s\n", script)
	}
	return b.String()
}

// ClassifyStrand picks the Episode's Strand from the canon, returning ""
// when nothing fits or when the canon is empty. The canon should be the
// Strands still in use: a Retired one accepts no new Airings, so
// classifying into it would place an Episode somewhere it can never go.
func ClassifyStrand(ctx context.Context, api API, canon []store.Strand, req StrandRequest) (string, Usage, error) {
	if len(canon) == 0 {
		return "", Usage{}, nil
	}
	raw, usage, err := api.CompleteJSON(ctx, strandModel, strandPrompt(canon, req), strandSchema(canon), strandMaxTokens)
	if err != nil {
		return "", usage, fmt.Errorf("classify strand: %w", err)
	}
	var out struct {
		Strand string `json:"strand"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", usage, fmt.Errorf("classify strand: output is not the agreed JSON: %w", err)
	}
	if out.Strand == "" || out.Strand == StrandNone {
		return "", usage, nil
	}
	// The enum should make this unreachable. Checking anyway: a strand
	// id that is not in the canon would be a dangling reference on a
	// public Episode, and a wrong-but-plausible id is exactly what a
	// fluent model produces when a schema is not enforced upstream.
	for _, s := range canon {
		if s.ID == out.Strand {
			return out.Strand, usage, nil
		}
	}
	return "", usage, fmt.Errorf("classify strand: %q is not in the canon", out.Strand)
}

// truncateRunes cuts s to at most n runes, never mid-rune.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
