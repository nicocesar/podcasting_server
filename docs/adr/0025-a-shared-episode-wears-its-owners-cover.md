# 25. A shared Episode wears its Owner's Cover Art

Date: 2026-07-27

## Status

Accepted

## Context

Cover Art has been per-Program since the beginning: one image per User
(ADR 0005, 0015), served as the RSS `<itunes:image>` on the *channel*
and on every web card. A Share places a reference to someone else's
Episode into a reader's Personal Feed (ADR 0006), and until now that
Episode was presented under **the receiving feed's** art. The
`episodePage` comment said why: the channel image was the only image a
feed had, and "no route exposes another user's cover inside this token
anyway."

That is wrong in the one place it matters most. When nico makes an
Episode and gives it to ldipenti, who forwards it to focagorda,
focagorda's podcast client shows it under *focagorda's* art — as though
they made it. The credit line carries the attribution on the web, but a
podcast client has no credit line: it has artwork and a title. The
Episode arrives looking like the reader's own work.

RSS has always been able to express this. `<itunes:image>` is valid
inside `<item>`, not only inside `<channel>`; Apple Podcasts, Spotify
and Pocket Casts honour it, and a client that ignores it falls back to
the channel image. The blocker was never the format — it was that we
had no address for another User's cover that a reader could legitimately
fetch.

## Decision

An Episode wears **its Owner's Cover Art wherever it lands**. Owner, not
Sharer: in the nico → ldipenti → focagorda chain, both recipients see
nico's art, because `Episode.OwnerID` is immutable and the Sharer is
not (ADR 0006). The art becomes provenance the forwarder cannot repaint.

Two new routes address another User's cover from inside the reader's own
namespace, mirroring the existing cover pair:

- `GET /f/{token}/u/{owner}/cover` — capability-addressed, `public`
  cache, what the RSS item points at.
- `GET /me/u/{owner}/cover` — the session twin, `private` cache, so
  signed-in markup carries no Feed Token (ADR 0008, 0013).

Both are one path segment deeper than the Episode Page
(`/f/{token}/{owner}/{file}`), so an Episode slugged `cover` cannot
collide with them.

Authorisation is `sharedOwner`: the Owner must actually have an Episode
in this reader's feed by way of a Share, and must not be muted. A
stranger's art 404s. This reveals nothing the feed did not already —
the reader can read the Episode, its title, and its audio.

The RSS item carries `<itunes:image>` **only when it differs from the
channel image**: a reader's own Episodes, and Owners who never uploaded
art, add no item image and fall back to the channel's. The web
surfaces — Episode Page, Dashboard rows, Player artwork — follow the
same rule, so `playerFor` now takes the cover rather than deriving it.

**Strands are explicitly out of scope.** An Episode delivered by a Follow
keeps wearing its Strand's art, exactly as before (ADR 0019); its Owner
may have shared nothing with this reader, so no owner-cover URL would
even resolve. Whether a public Strand should show per-Owner art is left
open.

## Considered Options

- **Embed the art in the MP3 as an ID3 `APIC` frame.** Rejected:
  Episode audio is a raw-byte concat of *bare* MP3 frames, deliberately
  stripped of headers so ElevenLabs' Info header cannot truncate
  playback. Prepending tags pokes at exactly that, and would mean
  rewriting stored audio per Share. The feed route touches no bytes.
- **Point the item image at the Owner's own cover URL.** Rejected: that
  URL is either a capability inside the Owner's namespace, which the
  reader must not hold, or a public address, which a private Episode has
  no business advertising.
- **Copy the Owner's image into the recipient at Share time.** Rejected
  for the same reason ADR 0006 rejected copying the Episode: it
  duplicates storage per Share and goes stale when the Owner replaces
  their art.
- **Leave it and rely on the credit line.** Rejected: podcast clients
  have no credit line, and the feed is the point.

## Consequences

- A shared Episode is visually attributable in a podcast client, not
  just on the web. Forwarding no longer launders authorship.
- Rendering a feed costs one `GetUser` per distinct Owner
  (`ownerCovers`), not one per Episode. Own-only feeds cost nothing new.
- A reader can fetch the Cover Art of anyone who shared with them. That
  image was already effectively visible to them on the Episode Page.
- An Owner replacing their cover changes the art on every Episode they
  have shared, everywhere, within the hour the cache allows — the same
  propagation ADR 0003 accepted.
- Clients that ignore item-level artwork are unchanged: they see the
  channel image they saw before.
- The Dashboard is now visually mixed — rows no longer share one cover.
  That is the intended signal, but it is a change in how a busy feed
  reads.
