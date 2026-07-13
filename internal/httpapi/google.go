package httpapi

// "Sign in with Google": a plain OIDC authorization-code flow against
// Google's endpoints, used three ways (the state's mode): logging into
// an existing linked account, linking a Google identity to the current
// session's account, and Redemption ("Join with Google" on the invite
// page). Google sign-in never creates an account by itself — an
// unrecognized identity is turned away (CONTEXT.md "Login").
//
// The id_token is fetched directly from Google's token endpoint over
// TLS with our client secret, so its signature needs no separate JWKS
// verification; aud, iss, and exp are still checked.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const (
	googleAuthURL    = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL   = "https://oauth2.googleapis.com/token"
	oauthStateTTL    = 10 * time.Minute
	oauthNonceCookie = "oauth_nonce"
)

type googleOIDC struct {
	clientID     string
	clientSecret string
	tokenURL     string // overridden in tests
}

// oauthState rides through Google and back, HMAC-signed so it cannot be
// forged and nonce-bound to the browser that started the flow.
type oauthState struct {
	Mode   string `json:"m"` // "login" | "link" | "redeem"
	Next   string `json:"next,omitempty"`
	User   string `json:"user,omitempty"`   // link: session user; redeem: chosen username
	Invite string `json:"invite,omitempty"` // redeem: the invite token
	Nonce  string `json:"n"`
	Expiry int64  `json:"exp"`
}

func (s *server) encodeState(st oauthState) string {
	b, _ := json.Marshal(st)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + s.sign(payload)
}

func (s *server) decodeState(v string) (oauthState, error) {
	payload, mac, ok := strings.Cut(v, ".")
	if !ok || !hashEqual(s.sign(payload), mac) {
		return oauthState{}, errors.New("bad state signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return oauthState{}, err
	}
	var st oauthState
	if err := json.Unmarshal(b, &st); err != nil {
		return oauthState{}, err
	}
	if time.Now().Unix() > st.Expiry {
		return oauthState{}, errors.New("state expired")
	}
	return st, nil
}

func (s *server) googleRedirectURI(r *http.Request) string {
	return s.base(r) + "/auth/google/callback"
}

// startGoogle sends the browser to Google's consent screen, minting the
// signed state and its nonce cookie.
func (s *server) startGoogle(w http.ResponseWriter, r *http.Request, st oauthState) {
	nonce, err := randomHex(16)
	if err != nil {
		s.fail(w, err)
		return
	}
	st.Nonce = nonce
	st.Expiry = time.Now().Add(oauthStateTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     oauthNonceCookie,
		Value:    nonce,
		Path:     "/auth/google",
		MaxAge:   int(oauthStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure(r),
		SameSite: http.SameSiteLaxMode,
	})
	q := url.Values{
		"client_id":     {s.google.clientID},
		"redirect_uri":  {s.googleRedirectURI(r)},
		"response_type": {"code"},
		"scope":         {"openid email"},
		"state":         {s.encodeState(st)},
		"prompt":        {"select_account"},
	}
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusSeeOther)
}

// handleGoogleStart begins a login (default) or, with ?link=1 and a live
// session, a link of the caller's account. Redemption starts from the
// invite page instead (handleRedeem).
func (s *server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("link") == "1" {
		u, ok := s.sessionUser(r)
		if !ok {
			s.unauthorized(w, r)
			return
		}
		s.startGoogle(w, r, oauthState{Mode: "link", User: u.ID})
		return
	}
	s.startGoogle(w, r, oauthState{Mode: "login", Next: r.URL.Query().Get("next")})
}

// googleClaims are the id_token fields this server uses.
type googleClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Aud           string `json:"aud"`
	Iss           string `json:"iss"`
	Exp           int64  `json:"exp"`
}

// exchange trades the authorization code for an id_token and validates
// its claims.
func (g *googleOIDC) exchange(ctx context.Context, code, redirectURI string) (googleClaims, error) {
	tokenURL := g.tokenURL
	if tokenURL == "" {
		tokenURL = googleTokenURL
	}
	form := url.Values{
		"code":          {code},
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return googleClaims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return googleClaims{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return googleClaims{}, fmt.Errorf("google token endpoint: %s", resp.Status)
	}
	var body struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return googleClaims{}, err
	}
	parts := strings.Split(body.IDToken, ".")
	if len(parts) != 3 {
		return googleClaims{}, errors.New("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return googleClaims{}, err
	}
	var claims googleClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return googleClaims{}, err
	}
	switch {
	case claims.Aud != g.clientID:
		return googleClaims{}, errors.New("id_token audience mismatch")
	case claims.Iss != "https://accounts.google.com" && claims.Iss != "accounts.google.com":
		return googleClaims{}, errors.New("id_token issuer mismatch")
	case time.Now().Unix() > claims.Exp:
		return googleClaims{}, errors.New("id_token expired")
	case claims.Sub == "" || !claims.EmailVerified:
		return googleClaims{}, errors.New("id_token without a verified identity")
	}
	return claims, nil
}

func (s *server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	st, err := s.decodeState(r.URL.Query().Get("state"))
	if err != nil {
		http.Error(w, "invalid or expired sign-in attempt — start over", http.StatusBadRequest)
		return
	}
	nonce, err := r.Cookie(oauthNonceCookie)
	if err != nil || !hashEqual(nonce.Value, st.Nonce) {
		http.Error(w, "sign-in attempt did not start in this browser — start over", http.StatusBadRequest)
		return
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		s.render(w, http.StatusOK, s.tmplLogin, loginPage{
			Error:         "Google sign-in was cancelled.",
			Next:          st.Next,
			GoogleEnabled: true,
		})
		return
	}
	claims, err := s.google.exchange(r.Context(), r.URL.Query().Get("code"), s.googleRedirectURI(r))
	if err != nil {
		s.log.Warn("google exchange failed", "err", err)
		http.Error(w, "Google sign-in failed — try again", http.StatusBadGateway)
		return
	}

	switch st.Mode {
	case "login":
		u, err := s.store.GetUserByGoogleSub(r.Context(), claims.Sub)
		if errors.Is(err, store.ErrNotFound) {
			s.render(w, http.StatusForbidden, s.tmplLogin, loginPage{
				Error:         "That Google account is not linked to any user here. Joining needs an invite.",
				Next:          st.Next,
				GoogleEnabled: true,
			})
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		s.setSession(w, r, u)
		http.Redirect(w, r, localPath(st.Next), http.StatusSeeOther)

	case "link":
		u, ok := s.sessionUser(r)
		if !ok || u.ID != st.User {
			s.unauthorized(w, r)
			return
		}
		if other, err := s.store.GetUserByGoogleSub(r.Context(), claims.Sub); err == nil && other.ID != u.ID {
			http.Error(w, "that Google account is already linked to another user", http.StatusConflict)
			return
		}
		u.GoogleSub, u.GoogleEmail = claims.Sub, claims.Email
		if err := s.store.UpsertUser(r.Context(), u); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, "/me", http.StatusSeeOther)

	case "redeem":
		s.finishGoogleRedemption(w, r, st, claims)

	default:
		http.Error(w, "bad state", http.StatusBadRequest)
	}
}

// finishGoogleRedemption is the second half of "Join with Google": the
// invite and username were validated before the Google round-trip, but
// both are re-checked — the world may have moved while the browser was
// away at the consent screen.
func (s *server) finishGoogleRedemption(w http.ResponseWriter, r *http.Request, st oauthState, claims googleClaims) {
	inv, err := s.store.GetInvite(r.Context(), st.Invite)
	if err != nil || !inv.Redeemable(time.Now()) {
		s.renderNotFound(w)
		return
	}
	if _, err := s.store.GetUserByGoogleSub(r.Context(), claims.Sub); err == nil {
		s.render(w, http.StatusConflict, s.tmplInvite, invitePage{
			Inviter: inv.InviterID,
			Error:   "That Google account already belongs to a user here — log in instead.",
		})
		return
	}
	username := st.User
	if _, err := s.store.GetUser(r.Context(), username); err == nil {
		data := s.invitePageData(r, inv)
		data.Error = "That username was taken while you were signing in — pick another."
		s.render(w, http.StatusConflict, s.tmplInvite, data)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.completeRedemption(w, r, inv, store.User{
		ID:          username,
		Title:       username,
		GoogleSub:   claims.Sub,
		GoogleEmail: claims.Email,
	})
}
