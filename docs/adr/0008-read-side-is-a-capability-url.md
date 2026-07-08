# 8. The read side is a capability URL, not Basic auth

Date: 2026-07-08

## Status

Accepted (retires the Reader/Basic-auth half of ADR 0005's credential
design; the publish token half stands)

## Context

The read credential was a 32-hex password entered into a podcast client's
Basic-auth dialog — hostile to type on a phone, and the single worst
moment of onboarding. Private-podcast platforms solved this years ago:
the subscribe URL itself carries an unguessable token.

## Decision

Each User has one **Feed Token**. The entire read side lives under
`/f/{token}/`: the feed (`feed.xml`), every audio enclosure
(`{owner}/{slug}.mp3`), the Cover Art (`cover`), a QR rendering of the
feed URL (`qr.png`), and a subscribe landing page. The URL is the
credential; podcast clients never see an auth dialog. The read password
is gone — a User's secrets are exactly two: Feed Token (read) and publish
token (write, Management API, Dashboard).

Subscribing is paste-a-URL, scan-a-QR, or tap an AntennaPod deep link —
the three are shown together wherever the feed URL appears (welcome page,
Dashboard, landing page). The token is rotated self-service from the
Dashboard: rotation risks nothing but read access, so it must not wait on
the operator. Admin credential rotation (ADR 0007) rotates both secrets.

## Considered Options

- **Userinfo-embedded URLs** (`https://user:pass@host/...`): one pasteable
  string, but client support for userinfo parsing is inconsistent and the
  Basic-auth machinery survives underneath. Rejected.
- **Keeping both mechanisms**: three secrets per user, two rotation
  stories, a second auth path to test forever. Rejected while the
  migration cost was one subscription.
- **Shorter passwords**: trades entropy for typability and still leaves
  the dialog. Rejected.

## Consequences

- The feed URL is a bearer secret: anyone who sees it can read the feed
  until it is rotated. `itunes:block` is already set; the mitigation is
  the one-click reset.
- Enclosure URLs are per-feed (`/f/{token}/{owner}/{slug}.mp3`) while the
  GUID stays `(owner, slug)`, so a rotated or shared episode is still the
  same item to clients.
- The Feed Token is stored as-is (not hashed): it must be displayed back
  to its owner as URL and QR. Same posture as the retired cover secret.
- Existing subscriptions on the Basic-auth URLs die at deploy; users
  provisioned before this change get a Feed Token minted on their first
  `/me` visit.
