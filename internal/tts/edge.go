package tts

import (
	"context"
	"fmt"

	"github.com/difyz9/edge-tts-go/pkg/communicate"
)

// Edge synthesizes through Microsoft Edge's "Read Aloud" service via
// edge-tts-go. Free and good-quality, but unofficial: Microsoft rotates
// the protocol occasionally and the library must catch up, which is why
// it is only ever the first engine in the chain, never the only one.
type Edge struct{}

func NewEdge() Edge { return Edge{} }

func (Edge) Name() string { return "edge-tts" }

func (Edge) Synthesize(ctx context.Context, text string, v Voice) ([]byte, error) {
	comm, err := communicate.NewCommunicate(
		text, v.Edge,
		"+0%", "+0%", "+0Hz", // rate, volume, pitch
		"",     // proxy
		10, 60, // connect / receive timeouts (seconds)
	)
	if err != nil {
		return nil, err
	}
	chunks, errs := comm.Stream(ctx)
	var out []byte
	for chunk := range chunks {
		if chunk.Type == "audio" {
			out = append(out, chunk.Data...)
		}
	}
	if err := <-errs; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no audio received")
	}
	return out, nil
}
