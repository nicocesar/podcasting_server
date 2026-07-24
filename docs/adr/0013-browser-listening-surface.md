# 13. Listening in the browser: an inline Player and an Episode Page

Date: 2026-07-23

## Status

Accepted

## Context

Every listening path so far ends in a podcast client: subscribe with the
Feed Token, let AntennaPod fetch the enclosure. That is right for the
habit, but wrong for the two moments that bracket it. An owner who has
just generated an episode wants to hear the first ten seconds before
sharing it, and a person handed a link wants to press play, not to
subscribe first. Both currently require downloading an MP3 by hand.

The Dashboard already lists Episodes; the feed landing page shows only
Cover Art and the subscribe box. `handleAudio` already serves enclosures
with `http.ServeContent`, so Range requests and seeking work today, and
`store.Episode` already carries `DurationSec`, `AudioSize`, and
`AudioType`. Nothing about the audio side needs to change — what is
missing is a surface.

The repository has no JavaScript dependencies, no bundler, and no Node
toolchain; every page is hand-written vanilla JS. Whatever we add has to
keep that true.

## Decision

**A Player, on two surfaces.** The Player is a small vanilla-JS module in
`static/player.js` with styles in `style.css`, loaded through the
existing `?v={{assetv}}` cache-buster. It appears in every Episode row on
the Dashboard, and on a new Episode Page.

**Progressive enhancement, one element.** The server renders
`<audio controls preload="none">`. The script removes the `controls`
attribute and builds our own UI around that same element, so the
`<audio>` element is the single source of truth either way and a blocked
or broken script leaves a working native player behind. `preload="none"`
means a list of forty Episodes costs zero audio bytes until someone
presses play; the scrubber is rendered at full length from the
server-known `DurationSec`, so it does not resize on `loadedmetadata`.

Controls are play/pause, a seekable scrubber showing the buffered range,
elapsed and total time, ±15s, and a speed cycle (1×, 1.25×, 1.5×, 2×).
No volume control: iOS ignores `volume` outright and desktop already has
an OS one. Media Session metadata and handlers are wired, so a tab can be
driven from a lock screen or a headset button. One coordinator enforces
that starting a Player pauses every other Player on the page.

**The Episode Page** shows the feed's Cover Art, the Episode's title, its
Description (rendered nowhere in HTML until now), publication date,
duration, the Player, and a direct download link. It has two addresses
for the same content, because there are two ways to be entitled to it:

- `/f/{token}/{owner}/{slug}` — the capability address, the same route
  that already serves `{slug}.mp3`, whose non-`.mp3` form was a 404. For
  whoever holds the Feed Token. Carries the subscribe fragment and the
  warning below.
- `/me/episodes/{owner}/{slug}` — the signed-in address, authorised by
  the session cookie the browser already has. This is what the Dashboard
  links and plays.

Both resolve the Episode by the same rule: the reader's own, or a
`GetShare` hit when the Owner is someone else.

The second address exists for privacy, not tidiness. A URL under `/f/`
*is* the whole feed, so while a signed-in person browses their own
Episodes, nothing in their address bar — and nothing they could copy,
bookmark, or leak — is a capability. The Feed Token appears on the
Dashboard only where it is the point: the subscribe box.

**Capability pages send `Referrer-Policy: no-referrer`.** Everything
under `/f/` sets it. Without it, following any link out of one of those
pages — a link inside an Episode description, say — would hand the Feed
Token to the destination site in the `Referer` header.

**The Episode Page is not a share link.** Its URL contains the Feed
Token, so pasting it hands over the whole Personal Feed (ADR 0008).
The page therefore offers no copy-link affordance and repeats the
Dashboard's warning that the URL is the key. Sharing one Episode remains
Share-to-username or an Invite carrying that Episode (ADR 0006, 0007).

**Resume Position is not domain state.** The Player records where you
stopped in `localStorage`, keyed by Owner and Slug, and offers to resume.
It never reaches the store.

## Considered Options

- **A vendored player library** (Plyr, media-chrome): correct
  accessibility and edge cases for free, at the cost of a vendored bundle
  to keep current and a look that is not ours. Rejected — the control set
  we want is small enough that the library is the larger liability.
- **Native `<audio controls>` alone**: zero code, but three different
  appearances across browsers, no hook for speed or resume, and no
  relationship to the design system. Kept only as the no-JS fallback.
- **Web Audio API with a canvas waveform**: requires decoding the whole
  MP3 client-side or precomputing peaks server-side. Rejected as
  expensive decoration for talk audio.
- **A docked player bar** that survives navigation: every page here is a
  full load, so playback would die on any click. Rejected until there is
  a reason to avoid reloads.
- **Per-Episode capability tokens** (`/e/{token}`), making Episode Pages
  genuinely pasteable: a new entity, an expiry policy, a revocation UI,
  and a near-duplicate of Invite. Rejected; Invite already answers "send
  this to one person".
- **Server-side playback progress**: syncs across devices and could mark
  Episodes played, but adds a domain entity plus a write-heavy endpoint
  to both backends, and still would not sync with the podcast client
  where most listening happens. Rejected.
- **An Episode list on the `/f/{token}` landing page**: would make it a
  full show page. Deferred — it is an independent change, not a
  prerequisite.
- **A JavaScript test runner**: a second toolchain in a Go repository for
  ~150 lines of DOM glue. Rejected; the Go tests cover the route and the
  rendered markup.

## Consequences

- A third HTML surface renders Episode metadata (feed, Dashboard,
  Episode Page). Description in particular is now user-visible in HTML
  and must be escaped as such.
- Dashboard rows grow by roughly one control strip each and the list is
  unpaginated (`ListEpisodes`), so a long-lived feed scrolls further.
  Collapsing the share controls behind a disclosure is the escape hatch
  if it bites.
- A shared-in Episode's page shows the *feed's* Cover Art, not the
  Owner's — the same substitution the RSS channel already makes.
- Playback position and speed live in one browser only. Clearing site
  data loses them, and that is acceptable: the same is true of the
  podcast client.
- `handleAudio` now serves two representations off one route. The
  `.mp3`-suffix check is what separates them and must stay strict.
- One Episode has two working URLs. They are not interchangeable: only
  the `/f/` one can be given to a podcast client, and only the `/me` one
  is safe to have in a browser's history. Anything rendering an Episode
  has to know which surface it is on.
- The capability granularity problem remains open: there is still no way
  to grant one Episode to someone without an account. `/me` addresses
  narrow what leaks *accidentally*; they do not add a way to share
  deliberately outside the membership graph. That is a separate decision.
