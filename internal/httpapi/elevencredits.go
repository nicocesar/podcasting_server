package httpapi

// The ElevenLabs balance on the Spend page.
//
// Spend answers "what did this cost" out of Anthropic's billed dollars.
// ElevenLabs is the other meter running, and it fails differently: an
// Anthropic overspend is a bill, while an exhausted ElevenLabs balance
// takes music off the air completely and quietly demotes speech to the
// fallback engines. The failure that prompted this arrived as a 401 in
// a generation trace, which is the last place an operator looks and the
// last moment it is useful.
//
// No dollars here on purpose. ElevenLabs reports credits, not currency,
// and inventing a rate would be the price table this project has
// already declined to keep.

import (
	"context"
	"sync"
	"time"

	"github.com/nicocesar/podcasting_server/internal/elevenlabs"
)

// creditTTL is how stale a balance may be. Credit drains over minutes
// of generation, not seconds, and every admin page render would
// otherwise be a round trip to another vendor.
const creditTTL = 5 * time.Minute

// creditCache holds the last balance read. A failed read is cached too,
// for the same interval: when ElevenLabs is down, an admin refreshing
// the page should not queue up a timeout each time.
type creditCache struct {
	mu      sync.Mutex
	at      time.Time
	sub     elevenlabs.Subscription
	err     error
	fetched bool
}

// creditView is what the templates render.
type creditView struct {
	// Configured is false without a key: the section explains itself
	// instead of showing an empty meter.
	Configured bool
	Tier       string
	Status     string
	// Used, Limit and Remaining are grouped for reading: these are
	// five- and six-figure numbers compared against each other, and
	// "39381 of 40000" is a puzzle where "39,381 of 40,000" is not.
	Used      string
	Limit     string
	Remaining string
	// UsedPercent drives the bar, the same shape the daily spend bars
	// use so the page reads as one thing.
	UsedPercent int
	// Low and Exhausted drive the warning. Exhausted is the state that
	// was already breaking generations before anyone noticed.
	Low       bool
	Exhausted bool
	// Resets is when the allowance refills, blank if unknown.
	Resets string
	// Error is a failed read, shown rather than swallowed: a balance
	// nobody could fetch is not a balance of zero.
	Error string
}

// elevenCredits returns the balance for the admin surfaces, cached.
func (s *server) elevenCredits(ctx context.Context) creditView {
	if s.elevenAPI == nil {
		return creditView{}
	}
	sub, err := s.credits.get(ctx, s.elevenAPI)
	if err != nil {
		return creditView{Configured: true, Error: "Could not read the ElevenLabs balance: " + err.Error()}
	}
	v := creditView{
		Configured:  true,
		Tier:        sub.Tier,
		Status:      sub.Status,
		Used:        commas(sub.Used),
		Limit:       commas(sub.Limit),
		Remaining:   commas(sub.Remaining()),
		UsedPercent: sub.UsedPercent(),
		Low:         sub.Low(),
		Exhausted:   sub.Exhausted(),
	}
	if t := sub.Resets(); !t.IsZero() {
		v.Resets = t.Format("2 Jan")
	}
	return v
}

func (c *creditCache) get(ctx context.Context, api *elevenlabs.Client) (elevenlabs.Subscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched && time.Since(c.at) < creditTTL {
		return c.sub, c.err
	}
	c.sub, c.err = api.Subscription(ctx)
	c.at, c.fetched = time.Now(), true
	return c.sub, c.err
}
