package generation

// Character extraction: a story episode's cast, distilled from the
// finished script so later Story Time generations can bring it back.
// Deliberately not an agent session — one plain, schema-constrained
// /v1/messages call on a small model; the script is already written and
// there is nothing to research.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const extractModel = "claude-haiku-4-5"

// extractMaxTokens comfortably fits a cast list; the schema keeps the
// output from wandering.
const extractMaxTokens = 2000

var charactersSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"characters": map[string]any{
			"type":        "array",
			"description": "The story's recurring characters, main ones first.",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "The character's name as used in the story."},
					"description": map[string]any{"type": "string", "description": "One or two sentences: who they are, their personality, and any detail a sequel must keep consistent."},
				},
				"required": []string{"name", "description"},
			},
		},
	},
	"required": []string{"characters"},
}

const extractPrompt = `Below is the script of a children's audio story. List its characters so a future story can bring them back: for each character, the name as the story uses it and one or two sentences capturing who they are, their personality, and any detail a sequel must keep consistent (species, quirks, relationships). List main characters first; skip one-off background figures. Answer in the language of the script.

Script:
`

// ExtractCharacters distills the cast from a story script's spoken text.
func ExtractCharacters(ctx context.Context, api API, script string) ([]store.Character, Usage, error) {
	raw, usage, err := api.CompleteJSON(ctx, extractModel, extractPrompt+script, charactersSchema, extractMaxTokens)
	if err != nil {
		return nil, usage, fmt.Errorf("extract characters: %w", err)
	}
	var out struct {
		Characters []store.Character `json:"characters"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, usage, fmt.Errorf("extract characters: output is not the agreed JSON: %w", err)
	}
	if len(out.Characters) == 0 {
		return nil, usage, fmt.Errorf("extract characters: no characters found")
	}
	return out.Characters, usage, nil
}
