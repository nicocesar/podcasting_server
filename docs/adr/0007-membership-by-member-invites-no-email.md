# 7. Membership is by member-minted Invites; no email anywhere

Date: 2026-07-08

## Status

Accepted

## Context

Users were provisioned only by the operator through the admin API, with
credentials shown once and no recovery short of delete-and-recreate. Real
adoption needs a way in that doesn't funnel through the operator — but the
product is private: the Public Surface enumerates nothing, and strangers
must not be able to join or probe.

## Decision

Any existing User can mint an **Invite**: a single-use, unguessable token
that expires after 7 days and is revocable while pending. An Invite may
optionally carry one Episode from the inviter's feed. Redemption is a
public page where the invitee picks their own username and receives their
credentials — shown exactly once, hashes stored, like admin provisioning.
The carried Episode lands as a normal Share (Sharer = inviter), so the new
feed has something to play on its first refresh.

There is no email in the system: no addresses stored, no delivery
infrastructure, no PII beyond the username. Consequently there is no
self-service credential recovery; the admin rotates a user's credentials
on request (a new endpoint returning fresh secrets once, leaving episodes,
shares, and the feed URL untouched). The admin provisioning path remains
as a fallback.

## Considered Options

- **Open signup.** Rejected: reverses the privacy posture — the whole
  Public Surface is built around Users not being enumerable or probeable.
- **Admin-only invites.** Rejected: every new user would stall on the
  operator, killing the person-to-person growth loop that sharing creates.
- **Email-based recovery.** Rejected: drags in delivery infrastructure and
  a PII store to serve a rare event a small community handles out-of-band.
- **Pre-issued recovery links / inviter-can-reset.** Rejected: a second
  live secret people will also lose, or a permanent power edge from
  inviter over invitee that the domain otherwise doesn't have.

## Consequences

- The Redemption page is the Public Surface's first interactive endpoint;
  username availability is only probeable by someone holding a valid
  Invite token.
- An unredeemed Invite is a live door into the system; expiry and
  revocation bound its lifetime. No per-user invite quota yet — add one if
  abuse appears.
- Losing credentials means asking the operator, accepted as the honest
  cost of having no email.
