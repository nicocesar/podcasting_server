package tts

import (
	"context"
	"encoding/base64"
	"fmt"

	texttospeech "google.golang.org/api/texttospeech/v1"
)

// Google synthesizes through Google Cloud Text-to-Speech, authenticating
// with application default credentials (the Cloud Run service account —
// no new secret). Official and fast; ~$16 per million characters, which
// is cents per episode. The API must be enabled on the project (see
// SETUP.md).
type Google struct {
	svc *texttospeech.Service
}

func NewGoogle(ctx context.Context) (*Google, error) {
	svc, err := texttospeech.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("tts: google: %w", err)
	}
	return &Google{svc: svc}, nil
}

func (*Google) Name() string { return "google-tts" }

func (g *Google) Synthesize(ctx context.Context, text string, v Voice) ([]byte, error) {
	resp, err := g.svc.Text.Synthesize(&texttospeech.SynthesizeSpeechRequest{
		Input: &texttospeech.SynthesisInput{Text: text},
		Voice: &texttospeech.VoiceSelectionParams{
			LanguageCode: v.GoogleLang,
			Name:         v.Google,
		},
		AudioConfig: &texttospeech.AudioConfig{AudioEncoding: "MP3"},
	}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(resp.AudioContent)
}
