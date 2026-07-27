# Podcasting Server

A private, multi-user podcast server: each User has one private Personal
Feed of generated, news-like audio briefings, consumed from a phone podcast
client, and can share individual Episodes with other Users. Episodes are
produced elsewhere; this context stores, lists, serves, and shares them.

## Language

### People & identity

**User**:
A person with an account: exactly one Personal Feed, a Login for the
webapp, API Keys for their Generators, and a Feed Token (used by their
podcast client).
_Avoid_: account, member, reader, writer

**Owner**:
The User whose Personal Feed an Episode was first published to — i.e. the
API Key that created it. Immutable for the Episode's lifetime.
_Avoid_: creator, author, uploader

**Sharer**:
The User whose Share placed an Episode into a particular Personal Feed.
May differ from the Owner, since any recipient may share onward.
_Avoid_: forwarder, sender

### Credentials

**Login**:
How a User enters the webapp: a password, a linked Google identity, or
both — at least one exists from Redemption onward. Google sign-in never
creates an account; an unrecognized Google identity is turned away. There
is no self-service password reset: a Google-linked User signs in with
Google and changes the password; otherwise the admin resets it.
_Avoid_: sign-in method, credentials (ambiguous)

**Session**:
The signed-in state of a browser after a Login, lasting weeks unless the
User logs out or logs out everywhere. The only path to Credential
Management.
_Avoid_: cookie, login token

**API Key**:
A named, individually revocable credential a User mints for one Generator.
Grants the Publishing Contract and the Management API but never Credential
Management. The plaintext is shown exactly once, at minting; a User may
hold many, and revoking one leaves the others working.
_Avoid_: publish token, personal access token, secret

**Credential Management**:
The security-sensitive operations reachable only from a Session, never
with an API Key: set or change the password, link or unlink Google, mint
or revoke API Keys, reset the Feed Token.
_Avoid_: account settings (broader), security page

### Feed & content

**Personal Feed**:
A User's single private RSS feed: a view over Episode references — their
own, those shared with them, and those a Follow delivers — never a
container holding copies.
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

**Strand**:
The one subject an Episode belongs to, chosen from a fixed canon the
station defines — not free text, and never coined by a User or by the
machine. An Episode has exactly one Strand or none; "none" is a
deliberate outcome, and the pile of Strandless Episodes is the evidence
for which Strand to add to the canon next. Assigned by the station, not
declared by the Owner (see Stranding). The canon is kept by the admin,
who names a Strand and gives it cover art; its id then never changes,
because that id is its public address. A Strand that has been Aired into
is never deleted, only Retired.
_Avoid_: tag, topic (that's the generation input), category, genre,
channel, sequence

**Retired Strand**:
A Strand that has left the canon without leaving the internet: it accepts
no new Airings and appears in no discovery, while its page and feed keep
serving whoever already subscribed. The only way a Strand ends, once
anything has Aired on it.
_Avoid_: archived, deleted, disabled

**Stranding**:
The station reading a finished Episode and placing it in a Strand: one
schema-constrained model call over the script, or over the Topic and
title where there is no script. The schema admits only the existing
canon, so a Strand cannot be invented by inference. Episodes arriving
through the Publishing Contract from an external Generator are not
Stranded — there is nothing to read but a title. The station proposes;
the Owner disposes, and may set or change the Strand when Airing.
_Avoid_: tagging, classification, auto-tagging

**Airing**:
The Owner's act of putting one of their Episodes on its Strand, where
anyone may hear it with no capability at all — the only way anything
leaves the private side of the station. Deliberately per-Episode and by
hand: the Owner has heard it before strangers can. An Episode with no
Strand cannot be Aired, because there would be nowhere for it to go. A
record of its own, not a flag: it carries the public identifier, and
un-Airing deletes it. Re-Airing mints a new identifier, so links killed
by an un-Air stay dead.
_Avoid_: publish (that's the Publishing Contract), post, go live

**Aired Episode**:
An Episode with a live Airing. Its audio and its page are reachable
without a Feed Token, it is attributed to its Owner by their feed title,
and it appears in its Strand's page and feed. The Owner's delete removes
it from the public surface as it does from every other feed; the Owner's
republish silently changes what the public hears, since an Airing refers
to the Episode, not to a snapshot of it.
_Avoid_: public episode, published episode

**Strand Page**:
A Strand rendered for a browser with no credentials: its cover art,
its description, its Aired Episodes newest first, and the subscribe URL
and QR for its Strand Feed. Where a link to an Aired Episode lands.
_Avoid_: strand home, category page

**Strand Feed**:
A Strand as an RSS feed anyone may subscribe to in a podcast client — no
token, no dialog, the one place in the station where audio is served
without a capability. Multi-author: the channel is the station, each item
is attributed to its Owner. Carries `itunes:block`, so it is reachable by
URL but listed in no directory.
_Avoid_: public feed, show, channel

**Cover Art**:
The single image associated with a Personal Feed, displayed by podcast
clients. Served inside the Feed Token namespace, so any client that can
read the feed can fetch the artwork the same way.
_Avoid_: artwork, thumbnail, logo

**Art Spec**:
The record of how a Strand's cover art was made: the words set on it, and
the colour and icon, each of which may be left to follow from the words.
Kept so the canon page can show what a Strand's art actually says rather
than guessing from its title, and so that saving a Strand redraws its art
only when the Spec changed. Emptied when art is uploaded instead of drawn,
which is how a generated cover and an uploaded one are told apart — the
two are otherwise interchangeable by design (ADR 0020). An empty Spec on a
Strand that has art means the art came from a file (ADR 0021).
_Avoid_: art settings, cover config, art metadata

**Episode Page**:
A single Episode rendered as HTML — Cover Art, title, description,
duration, Player, download link. It has two addresses for the same
content: `/me/episodes/{owner}/{slug}` for a signed-in browser, and the
capability form inside the Feed Token namespace (the audio address
without the `.mp3`). The Dashboard uses the first, so that a listener's
address bar never holds the key to their whole feed. The second is a
place to listen, *not* a share link: passing that URL on passes on the
whole Personal Feed. Sharing one Episode is still a Share or an Invite
(ADR 0013). On the signed-in address, and only for the Owner reading their
own Episode, the page also offers the Airing control the Dashboard offers —
the same control rendered in two places, never a second way to do it.
_Avoid_: episode permalink, public episode link, show notes page

**Player**:
The in-browser playback control shown on the Dashboard and the Episode
Page: play/pause, scrubber, ±15s, speed. A thin layer over the browser's
own `<audio>` element with no third-party code; without JavaScript the
native controls remain. A convenience surface — the podcast client is
still where listening mostly happens.
_Avoid_: media player, widget, embed

**Resume Position**:
Where a listener stopped inside an Episode in *this browser*. Deliberately
not domain state: it lives in browser storage only, is never sent to the
server, and does not follow the User to another device or into their
podcast client. Do not promote it to a stored record without revisiting
ADR 0013.
_Avoid_: playback progress, listen state, played marker

### Membership

**Invite**:
A link, minted by a User (or the admin), that for a set time does two
things: plays the one Episode it carries, and admits one new User. The
playing is unlimited; the admitting happens once. It can be revoked
while it lives — by whoever minted it, and by the Owner of the Episode
it carries. An Invite carrying no Episode is a plain door and nothing
else. The Episode is delivered as a Share (Sharer = inviter) at
Redemption.
_Avoid_: signup link, referral, access code, share link

**Guest**:
Someone holding a live Invite who has not redeemed it. A Guest can hear
the one Episode that Invite carries and learn nothing else — not the
feed it came from, not what else is in it, not that anything else
exists. Guests are not Users: Blocks and Mutes are user-to-user and do
not reach them, so revoking the Invite is the only control over a Guest.
_Avoid_: anonymous user, visitor, listener

**Redemption**:
The act of turning an Invite into a User on the public invite page: the
invitee picks their username, establishes their Login (sets a password or
links Google, their choice), and receives their feed URL. The only way to
join — there is no open signup, and Google sign-in alone never creates an
account.
_Avoid_: registration, signup, onboarding

### Sharing

**Share**:
The act of placing a reference to an Episode into another User's Personal
Feed, addressed by username, landing immediately — no inbox or approval.
Any User with the Episode in their feed may Share it onward; the Episode
remains the Owner's, and the Owner's replace or delete propagates to every
feed referencing it.
_Avoid_: send, forward, repost

**Vouch**:
One signed-in User putting their name to one Aired Episode: public,
attributed by feed title, and never for one's own Episode. Not a vote —
there is no ranking, no score and no ordering by it. At the size of this
station a Vouch says "this one is worth your time", and one of them is
worth more than a tally would be.
_Avoid_: vote, like, upvote, rating

**Follow**:
A User's standing choice to have a Strand's Aired Episodes delivered into
their Personal Feed. The third kind of reference a Personal Feed holds,
after the User's own Episodes and their Shares — and unlike a Share it
has no Sharer, because nobody chose to send it. Unfollowing is the
control; Block and Mute are not overloaded to do this job.
_Avoid_: subscribe (that's a podcast client and a feed URL), watch

**Bar**:
The number of Vouches an Episode must carry before a Follow delivers it:
zero for everything Aired, one — the default — for whatever somebody
vouched for, higher for a Strand that has become noisy. Set per Follow,
so the same Strand can be a firehose for one listener and a trickle for
another.
_Avoid_: threshold, score, filter

**Settling**:
The day between Airing and eligibility, during which Vouches accumulate.
When it ends the Vouch count is frozen onto the Airing and the delivery
question is answered once and for all: above the Bar it goes out, below
it never does. Later Vouches still show, but they no longer deliver —
because nothing may be inserted into a listener's past, and re-dating an
Episode would misdate the news inside it.
_Avoid_: cooldown, delay, embargo

**Block**:
A recipient control: Shares from a Blocked User never enter my Personal
Feed again. Targets the Sharer, not the content.
_Avoid_: ban, unfollow

**Mute**:
A recipient control: Episodes owned by a Muted User never appear
anywhere I look — not in my Personal Feed however they got there, and
not on a Strand Page I browse while signed in. Targets the Owner.
_Avoid_: hide, filter out

### Generation

**Generation**:
A User-requested production of one Episode from a Topic: research anchored
in the Freshness Window, a Script at the Target Length in the chosen
Language, voicing, and publication into the requester's own Personal Feed.
Progress is observable stage by stage; it ends published or failed, and a
failed Generation can be retried from the last completed stage without
redoing finished work.
_Avoid_: job, task, run

**Generation Template**:
One of the programs the station can produce — The Briefing, Story Time,
The Long Room — each with its own voice, its own form fields, and its own
idea of what a good episode is. A User picks one before anything else;
everything downstream of the Script is the same whichever they picked.
_Avoid_: program type, genre, mode

**Topic**:
The free-text subject a User submits to start a Generation — the only
creative input; everything else is chosen from fixed options.
_Avoid_: prompt, query

**Beat**:
A Topic a User has the station cover on an ongoing basis: a standing
request that produces a new Episode into their Personal Feed at a fixed
cadence, until they pause or cancel it. A Beat holds a frozen copy of
everything a Generation needs, so every Episode it makes is the same
request asked again. It is dormant between Episodes — it comes round when
the User's Personal Feed is polled or their Dashboard opened, so a Beat
nobody is listening to falls quiet, and one that has been quiet a while
covers the whole gap when it wakes.
_Avoid_: schedule, cron job, recurrence, series, subscription

**Freshness Window**:
The trailing time span (one day to one year) a Generation is anchored in:
the developments the Episode covers are sourced from within it, older
material may provide background, and the Episode says so when the window
holds little on the Topic. A soft bound — a thin window never fails a
Generation.
_Avoid_: recency, date filter, lookback

**Target Length**:
The requested spoken duration of a generated Episode, chosen from fixed
options (2 to 60 minutes). A target the Script aims at, not a guarantee;
the published Episode's duration is still measured by the server.
_Avoid_: duration (that's the measured one)

**Language**:
The User-chosen language of a generated Episode, picked per Generation
from a curated list: the Script is written in it and the Episode is voiced
in it.
_Avoid_: locale, voice (the voice follows from the Language)

**Script**:
The complete text of a generated Episode as it is to be spoken, together
with its title, summary, and sources. The durable midpoint of a
Generation: once written, a later failure never requires researching or
writing again.
_Avoid_: transcript, draft

### Interfaces

**Generator**:
Any actor that produces Episodes for a User and delivers them through the
Publishing Contract. Two kinds exist: an external service authenticating
with one of the User's API Keys (out of scope except for the contract it
must honor), and the built-in Generation the server runs on the User's
request.
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
assets, the login page, the Redemption page for a valid Invite token,
and — since Airing — the Strand Pages, the Strand Feeds, and the audio
of Aired Episodes. Everything else requires a capability (Feed Token,
Invite), a Session, or an API Key. Strands are enumerable and are meant
to be; Users and Personal Feeds are not, which is why an Aired Episode
is addressed by an opaque Airing id and never by its Owner's username.
Reachable by anyone does not mean identical for everyone: a Strand Page
renders controls that depend on who is reading it — Follow and its bar,
Vouch, and the admin takedown — while serving the same Episodes to all.
A signed-in rendering is never publicly cached, which is what keeps the
difference safe (ADR 0023).
_Avoid_: public site, anonymous access
