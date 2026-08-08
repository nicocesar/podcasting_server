// Command audition builds a listening page for casting decisions.
//
// The storyVoices table in internal/tts is curated by ear — "this is
// listening work, not programming work" — and the listening has until now
// meant clicking through the ElevenLabs web library one voice at a time,
// where every clip is a different sentence at a different level and none
// of them is the thing the station actually renders.
//
// This renders candidates the way an episode will hear them: through
// text-to-dialogue, in eleven_v3, answering the pinned narrator of their
// language, all on one page. A voice that cannot be rendered that way is
// not a candidate at all, whatever the shared library says about it, so
// the failures are as much of the output as the successes.
//
// Nothing here runs in production. It spends a little TTS budget and
// writes an HTML file; the ids that survive the listening get pasted into
// storyVoices by hand, which is the point — the table's premise is that a
// human chose every entry.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicocesar/podcasting_server/internal/tts"
)

func main() {
	out := flag.String("out", "audition.html", "where to write the listening page")
	lang := flag.String("language", "en", "language to audition, as a BCP-47 primary tag")
	font := flag.String("font", "cmd/server/static/fonts/fraunces-var.woff2", "display face to inline")
	perSlot := flag.Int("per-slot", 6, "candidates to render per slot")
	dry := flag.Bool("dry-run", false, "list candidates without spending anything on audio")
	flag.Parse()

	key := os.Getenv("ELEVENLABS_API_KEY")
	if key == "" {
		fail("ELEVENLABS_API_KEY is not set")
	}
	slots, ok := slotsFor(*lang)
	if !ok {
		fail("no audition slots for language %q; add them to slotsFor", *lang)
	}
	if _, ok := scripts[*lang]; !ok {
		fail("no audition script for language %q; add one to scripts", *lang)
	}
	currentLang = *lang

	c := &client{key: key, http: &http.Client{Timeout: 120 * time.Second}}
	ctx := context.Background()

	page := page{Language: *lang, Generated: time.Now().UTC().Format(time.RFC1123)}
	if !*dry {
		css, err := fontCSS(*font)
		if err != nil {
			fail("%v", err)
		}
		page.FontCSS = css
	}
	seen := castVoiceIDs()
	for _, s := range slots {
		cands, err := c.candidates(ctx, s, *perSlot, seen)
		if err != nil {
			fail("%s: %v", s.Label, err)
		}
		fmt.Printf("%-28s %d candidates\n", s.Label, len(cands))
		if *dry {
			for _, v := range cands {
				fmt.Printf("    %-44s %s %s %s\n", trunc(v.Name, 44), v.Age, v.Descriptive, v.VoiceID)
			}
			continue
		}
		page.Slots = append(page.Slots, render(ctx, c, s, cands))
	}
	if *dry {
		return
	}

	page.Cost = 0
	for _, s := range page.Slots {
		for _, t := range s.Takes {
			page.Cost += t.Chars
		}
	}
	if err := write(*out, page); err != nil {
		fail("%v", err)
	}
	fmt.Printf("\nwrote %s — %d takes, %d characters billed\n", *out, page.takes(), page.Cost)
}

// slot is one casting gap: the part being filled, and the searches that
// might fill it. Several queries per slot because the shared library has
// no "child" age — only young/middle_aged/old — so a child voice is found
// by use case and by free text, never by one clean filter.
type slot struct {
	Label    string // human, for the page
	Role     string // the canonical role this fills
	Register string // "female" or "male"; empty means either
	Note     string // why this slot is open, shown on the page
	Queries  []url.Values
}

func q(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

// slotsFor returns the open casting gaps for a language. English first:
// it has no female child voice at all, and child, small_squeaky and silly
// are all one actor, so two children in one story are the same voice.
func slotsFor(lang string) ([]slot, bool) {
	switch lang {
	case "en":
		return []slot{
			{
				Label: "child · female", Role: "child", Register: "female",
				Note: "The blocking gap: no female child voice exists, so a girl is cast as Wilf.",
				Queries: []url.Values{
					q("language", "en", "gender", "female", "age", "young", "use_cases", "characters_animation", "descriptive", "cute"),
					q("language", "en", "gender", "female", "age", "young", "use_cases", "characters_animation"),
					q("language", "en", "gender", "female", "use_cases", "characters_animation", "search", "girl"),
				},
			},
			{
				Label: "child · male", Role: "child", Register: "male",
				Note: "A second boy, so two boys in one story are not both Wilf.",
				Queries: []url.Values{
					q("language", "en", "gender", "male", "age", "young", "use_cases", "characters_animation", "descriptive", "cute"),
					q("language", "en", "gender", "male", "age", "young", "use_cases", "characters_animation"),
					q("language", "en", "gender", "male", "use_cases", "characters_animation", "search", "boy"),
				},
			},
			{
				Label: "small_squeaky · either", Role: "small_squeaky",
				Note: "A creature, not a child: the duck that ADR 0032 caught sounding like the narrator.",
				Queries: []url.Values{
					q("language", "en", "use_cases", "characters_animation", "search", "squeaky"),
					q("language", "en", "use_cases", "characters_animation", "search", "cartoon animal"),
					q("language", "en", "use_cases", "characters_animation", "descriptive", "funny"),
				},
			},
			{
				Label: "silly · either", Role: "silly",
				Note: "The comic part, currently Wilf again — a third role sharing one actor.",
				// descriptive=funny reads as a loose tag on this endpoint and
				// returns professional narrators; free text finds the actual
				// comic voices.
				Queries: []url.Values{
					q("language", "en", "use_cases", "characters_animation", "search", "silly"),
					q("language", "en", "use_cases", "characters_animation", "search", "goofy"),
				},
			},
		}, true
	}
	return nil, false
}

// script is what each candidate performs: the language's pinned narrator
// setting up, then the candidate answering. Hearing the two together is
// the whole point — a candidate that sounds like the narrator is miscast
// however good it sounds alone.
type script struct {
	Narrator  string
	Candidate string
}

var scripts = map[string]script{
	"en": {
		Narrator:  "Emily leaned right over the edge of the fountain.",
		Candidate: "[giggling] Look at the water! Can I pet your dog?",
	},
}

type client struct {
	key  string
	http *http.Client
}

// sharedVoice is the slice of the shared-library record worth showing.
type sharedVoice struct {
	VoiceID     string `json:"voice_id"`
	Name        string `json:"name"`
	Gender      string `json:"gender"`
	Age         string `json:"age"`
	Accent      string `json:"accent"`
	Descriptive string `json:"descriptive"`
	UseCase     string `json:"use_case"`
	Description string `json:"description"`
}

// candidates runs every query for a slot and merges the results, dropping
// anything already cast in storyVoices — the table is what we are trying
// to widen, so a voice already in it is not a candidate.
//
// seen carries across slots as well as across the queries within one, so
// a voice that answers both "silly" and "squeaky" is rendered, paid for
// and listened to once. It lands in the first slot that finds it, which
// is why the blocking gaps are declared first.
func (c *client) candidates(ctx context.Context, s slot, limit int, seen map[string]bool) ([]sharedVoice, error) {
	var out []sharedVoice
	for _, params := range s.Queries {
		params.Set("page_size", "12")
		vs, err := c.shared(ctx, params)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			if seen[v.VoiceID] {
				continue
			}
			if s.Register != "" && v.Gender != s.Register {
				continue
			}
			seen[v.VoiceID] = true
			out = append(out, v)
			if len(out) == limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func (c *client) shared(ctx context.Context, params url.Values) ([]sharedVoice, error) {
	u := "https://api.elevenlabs.io/v1/shared-voices?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("shared-voices: %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var body struct {
		Voices []sharedVoice `json:"voices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Voices, nil
}

// dialogue renders one take through the same endpoint, model and output
// format an episode uses, so what you hear on the page is what the story
// will sound like rather than a vendor demo clip.
func (c *client) dialogue(ctx context.Context, narratorID, candidateID string, s script) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"inputs": []map[string]string{
			{"text": s.Narrator, "voice_id": narratorID},
			{"text": s.Candidate, "voice_id": candidateID},
		},
		"model_id": "eleven_v3",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.elevenlabs.io/v1/text-to-dialogue?output_format=mp3_44100_128", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return io.ReadAll(resp.Body)
}

// castVoiceIDs is every ElevenLabs id the server already casts, in any
// role or language.
func castVoiceIDs() map[string]bool {
	seen := map[string]bool{}
	for _, r := range tts.Roles {
		for _, l := range tts.Languages() {
			if v, ok := tts.RoleVoice(r.ID, l.Language); ok && v.Eleven != "" {
				seen[v.Eleven] = true
			}
		}
	}
	return seen
}

type page struct {
	Language  string
	Generated string
	Cost      int
	FontCSS   template.CSS
	Slots     []slotResult
}

// fontCSS inlines the station's display face. A data URI rather than a
// link because the page is opened from a file and published as a
// standalone artifact, with no /static to fetch from in either case.
func fontCSS(path string) (template.CSS, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("display face: %w (pass -font)", err)
	}
	return template.CSS(`src: url(data:font/woff2;base64,` +
		base64.StdEncoding.EncodeToString(b) + `) format("woff2");`), nil
}

func (p page) takes() int {
	n := 0
	for _, s := range p.Slots {
		n += len(s.Takes)
	}
	return n
}

type slotResult struct {
	slot
	Takes  []take
	Failed []failure
}

type take struct {
	sharedVoice
	Audio template.URL // data: URI, so the page is one self-contained file
	Chars int
}

// CleanName is what goes in the Go table. Shared-library names are sales
// copy — "Emily - Bright & Energetic", "Caroline Braden | warm read" —
// and the table wants the person, since for the narrator slot this string
// is read aloud in the station credit.
func (t take) CleanName() string {
	name := t.Name
	for _, sep := range []string{" - ", " – ", " | ", ", ", " ("} {
		if i := strings.Index(name, sep); i > 0 {
			name = name[:i]
		}
	}
	return strings.TrimSpace(name)
}

type failure struct {
	sharedVoice
	Err string
}

// render synthesizes every candidate in a slot, four at a time. A voice
// the dialogue endpoint refuses is reported rather than dropped: "this
// one is not usable for dialogue" is a casting fact worth seeing.
func render(ctx context.Context, c *client, s slot, cands []sharedVoice) slotResult {
	res := slotResult{slot: s}
	narrator, ok := tts.RoleVoice("narrator", currentLang)
	if !ok {
		fail("no narrator voice for %q", currentLang)
	}
	sp := scripts[currentLang]
	chars := len([]rune(sp.Narrator)) + len([]rune(sp.Candidate))

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, v := range cands {
		wg.Add(1)
		go func(v sharedVoice) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			audio, err := c.dialogue(ctx, narrator.Eleven, v.VoiceID, sp)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Printf("    ✗ %-40s %v\n", trunc(v.Name, 40), err)
				res.Failed = append(res.Failed, failure{sharedVoice: v, Err: err.Error()})
				return
			}
			fmt.Printf("    ✓ %-40s %6d bytes\n", trunc(v.Name, 40), len(audio))
			res.Takes = append(res.Takes, take{
				sharedVoice: v,
				Audio:       template.URL("data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(audio)),
				Chars:       chars,
			})
		}(v)
	}
	wg.Wait()
	// Concurrency makes the order arbitrary; the page should not shuffle
	// between runs of the same slot.
	sort.Slice(res.Takes, func(i, j int) bool { return res.Takes[i].Name < res.Takes[j].Name })
	return res
}

// currentLang is set once from the flag. A package variable rather than
// another parameter threaded through render: this is a one-off tool with
// one language per invocation.
var currentLang = "en"

func write(path string, p page) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pageTemplate.Execute(f, p)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "audition: "+format+"\n", args...)
	os.Exit(1)
}
