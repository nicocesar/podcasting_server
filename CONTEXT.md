# Podcasting Server

A private, multi-user podcast server: each User has one private Personal
Feed of generated, news-like audio briefings, consumed from a phone podcast
client, and can share individual Episodes with other Users. Episodes are
produced elsewhere; this context stores, lists, serves, and shares them.

## Language

### People & identity

**User**:
A person with an account: exactly one Personal Feed, a publish token (used
by their Generator), and a Feed Token (used by their podcast client).
_Avoid_: account, member, reader, writer

**Owner**:
The User whose Personal Feed an Episode was first published to — i.e. the
publish token that created it. Immutable for the Episode's lifetime.
_Avoid_: creator, author, uploader

**Sharer**:
The User whose Share placed an Episode into a particular Personal Feed.
May differ from the Owner, since any recipient may share onward.
_Avoid_: forwarder, sender

### Feed & content

**Personal Feed**:
A User's single private RSS feed: a view over Episode references — their
own plus those shared with them — never a container holding copies.
_Avoid_: show, channel, subscription

**Feed Token**:
The unguessable capability that is the entire read side: whoever holds
the feed URL can read the Personal Feed, its audio, and its Cover Art —
no password, no login dialog. Shown as a URL and a QR code; the owner can
reset it at any time, which kills the old URL instantly.
_Avoid_: read credential, reader password, feed password

**Episode**:
One playable item: an MP3 plus its metadata (title, description holding the
full generated summary text, publication time, optional duration). Exists
once, under its Owner; other Personal Feeds reference it. Episodes never
expire; they are news-like, so date and time-of-day are meaningful.
Identity is (Owner, Slug).
_Avoid_: item, track, file

**Slug**:
The unique identifier of an Episode within its Owner's Personal Feed; a
free-form string, by convention `YYYY-MM-DD-<day-part>` with optional
suffixes (e.g. `2026-07-06-morning-update1`). Publishing an existing Slug
replaces that Episode everywhere it is referenced.
_Avoid_: episode id, filename

**Day-part**:
A conventional time-of-day label used in Slugs: `morning`, `noon`,
`evening`, `night`. A naming convention only — the server does not validate
or enumerate it.
_Avoid_: edition, period

**Feed Variant**:
A filtered rendering of a Personal Feed at the same endpoint (only mine,
only shared with me, only from one User). Same credentials, same Episodes,
narrower view.
_Avoid_: sub-feed, playlist, smart feed

**Cover Art**:
The single image associated with a Personal Feed, displayed by podcast
clients. Served inside the Feed Token namespace, so any client that can
read the feed can fetch the artwork the same way.
_Avoid_: artwork, thumbnail, logo

### Membership

**Invite**:
A single-use token, minted by a User (or the admin), that admits exactly
one new User; it expires after a set time and can be revoked by its
inviter while pending. May carry one Episode from the inviter's feed,
delivered as a Share (Sharer = inviter) at Redemption.
_Avoid_: signup link, referral, access code

**Redemption**:
The act of turning an Invite into a User on the public invite page: the
invitee picks their username and receives their feed URL and publish
token, shown exactly once. The only way to join — there is no open
signup.
_Avoid_: registration, signup, onboarding

### Sharing

**Share**:
The act of placing a reference to an Episode into another User's Personal
Feed, addressed by username, landing immediately — no inbox or approval.
Any User with the Episode in their feed may Share it onward; the Episode
remains the Owner's, and the Owner's replace or delete propagates to every
feed referencing it.
_Avoid_: send, forward, repost

**Block**:
A recipient control: Shares from a Blocked User never enter my Personal
Feed again. Targets the Sharer, not the content.
_Avoid_: ban, unfollow

**Mute**:
A recipient control: Episodes owned by a Muted User never appear in my
Personal Feed, regardless of who Shares them. Targets the Owner.
_Avoid_: hide, filter out

### Interfaces

**Generator**:
The external actor (a separate, future service) that produces Episodes and
publishes them through the Publishing Contract, authenticating with one
User's publish token. Out of scope here except for the contract it must
honor.
_Avoid_: producer, worker, cron job

**Publishing Contract**:
The agreed interface through which a Generator delivers Episodes into the
authenticated User's own Personal Feed — the only way content enters the
system; the server owns all storage.
_Avoid_: upload API, ingestion

**Management API**:
The User-facing self-service operations: feed settings and Cover Art,
Share, remove a shared Episode from my feed, Block, Mute, and delete own
Episodes. Distinct from the read-side endpoints the podcast client
consumes.
_Avoid_: admin panel, backoffice

**Public Surface**:
The endpoints reachable with no secret at all: the landing page, static
assets, and the Redemption page for a valid Invite token. Everything else
requires a capability (Feed Token, Invite) or the publish token. The
landing page lists nothing, so neither Users nor feeds are enumerable.
_Avoid_: public site, anonymous access
