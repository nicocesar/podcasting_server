# 10. Split the write credential: API Keys for Generators, sessions for the webapp

Date: 2026-07-12

## Status

Accepted (retires the publish-token half of ADR 0005's credential design;
amends the Redemption flow of ADR 0007; the Feed Token of ADR 0008
stands)

## Context

One secret did all write-side work: a 48-hex publish token used as a
Basic-auth password by Generators *and* typed into a browser dialog to
reach the Dashboard. Secure, but hostile to humans — unmemorable,
untypeable on a phone, shown exactly once at Redemption, and all-powerful
if leaked from the remote box a Generator runs on. The prototype's data
is disposable, so no migration constrains the redesign.

## Decision

Two credentials for two audiences, with usernames staying the account
identity:

- **API Keys** (`pods_{keyid}_{secret}`, `Authorization: Bearer`) are
  the Generator credential: named, multiple per User, minted from the
  Dashboard with the plaintext shown once, individually revocable, only
  SHA-256 hashes stored. A key grants the Publishing Contract and the
  Management API.
- **Sessions** are the browser credential: log in with username +
  password (bcrypt) or a linked Google identity (OIDC code flow, matched
  strictly by `sub` — never by email). The session is a signed stateless
  cookie carrying a per-user credential version; bumping the version
  ("log out everywhere", password change, admin reset) kills every
  outstanding session with no session storage at all.
- **Credential Management is session-only**: minting/revoking keys,
  password changes, Google linking, and Feed Token resets refuse API
  keys with a 403. A leaked agent key can spam a feed but can never
  widen itself, move the feed URL, or lock the owner out.

Membership stays invite-only (ADR 0007): Redemption now establishes the
Login — set a password or complete Google sign-in right there (the
invite token rides through OAuth `state`) — and ends logged in. Google
sign-in never creates an account; an unrecognized identity is turned
away.

There is no self-service password reset, because there is no email in
this system and adding outbound mail infrastructure for this one flow is
not worth it: a Google-linked User signs in with Google and changes the
password; otherwise the operator issues a temporary password
(`POST /admin/users/{id}/password-reset`).

## Consequences

- Generators change one line: Basic auth becomes a Bearer header, and
  rotating one agent's key no longer breaks the others.
- Passwordless (Google-only) accounts exist; unlinking Google or any
  operation that would leave zero Logins is refused.
- `GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET` absent degrades cleanly to
  password-only; `SESSION_SECRET` becomes a deploy-time secret whose
  rotation logs every browser out.
- The Welcome page no longer shows a once-only secret — losing it stopped
  mattering, which was half the point.
