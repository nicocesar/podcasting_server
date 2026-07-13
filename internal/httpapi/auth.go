package httpapi

// Authentication (see CONTEXT.md "Credentials" and ADR 0010): two ways
// in, one authedHandler out. Generators present an API Key as a Bearer
// token; browsers carry a signed, stateless session cookie established
// at /login or Redemption. Credential Management is session-only.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const (
	sessionCookie = "session"
	sessionTTL    = 30 * 24 * time.Hour
	// apiKeyPrefix makes keys self-identifying in configs, logs, and
	// secret scanners: pods_{keyID}_{secret}.
	apiKeyPrefix = "pods_"

	minPasswordLen = 8
)

// secretHash is the stored form of an API Key secret: hex SHA-256.
func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// --- session cookie ---

// The cookie value is "userID:credVersion:expiryUnix:hmac" — stateless,
// so it works identically on fsstore and Datastore. A session dies when
// it expires or when the user's CredentialVersion moves past the one
// stamped here (password change, "log out everywhere").
func (s *server) sign(payload string) string {
	m := hmac.New(sha256.New, s.sessionSecret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *server) sessionValue(u store.User, expiry time.Time) string {
	payload := fmt.Sprintf("%s:%d:%d", u.ID, u.CredentialVersion, expiry.Unix())
	return payload + ":" + s.sign(payload)
}

// secure reports whether the request arrived over TLS (directly or via
// the Cloud Run proxy), which decides the cookie's Secure flag.
func (s *server) secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" ||
		strings.HasPrefix(s.baseURL, "https://")
}

func (s *server) setSession(w http.ResponseWriter, r *http.Request, u store.User) {
	expiry := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sessionValue(u, expiry),
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		Secure:   s.secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// sessionUser resolves the session cookie to its User: signature,
// expiry, and CredentialVersion must all hold.
func (s *server) sessionUser(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.User{}, false
	}
	parts := strings.Split(c.Value, ":")
	if len(parts) != 4 {
		return store.User{}, false
	}
	payload := strings.Join(parts[:3], ":")
	if !hashEqual(s.sign(payload), parts[3]) {
		return store.User{}, false
	}
	version, err1 := strconv.ParseInt(parts[1], 10, 64)
	expiry, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || time.Now().Unix() > expiry {
		return store.User{}, false
	}
	u, err := s.store.GetUser(r.Context(), parts[0])
	if err != nil || u.CredentialVersion != version {
		return store.User{}, false
	}
	return u, true
}

// --- API keys on the wire ---

// bearerUser resolves "Authorization: Bearer pods_{keyID}_{secret}" to
// the key's owner.
func (s *server) bearerUser(r *http.Request) (store.User, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return store.User{}, false
	}
	rest, ok := strings.CutPrefix(token, apiKeyPrefix)
	if !ok {
		return store.User{}, false
	}
	keyID, secret, ok := strings.Cut(rest, "_")
	if !ok {
		return store.User{}, false
	}
	k, err := s.store.GetAPIKey(r.Context(), keyID)
	if err != nil || !hashEqual(secretHash(secret), k.SecretHash) {
		return store.User{}, false
	}
	u, err := s.store.GetUser(r.Context(), k.UserID)
	if err != nil {
		return store.User{}, false
	}
	return u, true
}

// --- middleware ---

// wantsHTML distinguishes a browser from an API caller for error shape:
// browsers get redirected to /login, everything else gets a plain 401.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (s *server) unauthorized(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && wantsHTML(r) {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// originOK is the CSRF check for cookie-authenticated writes: SameSite=Lax
// keeps modern browsers from sending the cookie cross-site, and any
// Origin header that is present must match the request host.
func originOK(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	o, err := url.Parse(origin)
	return err == nil && o.Host == r.Host
}

// auth admits either credential: a Bearer API Key (Generators) or a
// session cookie (browsers). The read side still does not authenticate
// at all — it lives under /f/{token} (ADR 0008).
func (s *server) auth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, ok := s.bearerUser(r); ok {
			h(w, r, u)
			return
		}
		if u, ok := s.sessionUser(r); ok {
			if !originOK(r) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
			h(w, r, u)
			return
		}
		s.unauthorized(w, r)
	}
}

// session admits only a session cookie: Credential Management is out of
// an API Key's reach by construction, so a leaked key can never mint
// keys, change the password, or reset the Feed Token.
func (s *server) session(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.bearerUser(r); ok {
			http.Error(w, "credential management requires a browser session, not an API key", http.StatusForbidden)
			return
		}
		u, ok := s.sessionUser(r)
		if !ok {
			s.unauthorized(w, r)
			return
		}
		if !originOK(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		h(w, r, u)
	}
}

// --- login / logout ---

type loginPage struct {
	Username      string
	Error         string
	Next          string
	GoogleEnabled bool
}

// localPath keeps ?next= redirects on this site.
func localPath(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/me"
}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionUser(r); ok {
		http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, s.tmplLogin, loginPage{
		Next:          r.URL.Query().Get("next"),
		GoogleEnabled: s.google != nil,
	})
}

// dummyHash keeps a login attempt against a missing or passwordless
// account as slow as one against a real bcrypt hash.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("no such user"), bcrypt.DefaultCost)

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")
	next := r.FormValue("next")

	fail := func() {
		s.render(w, http.StatusUnauthorized, s.tmplLogin, loginPage{
			Username:      username,
			Error:         "Wrong username or password.",
			Next:          next,
			GoogleEnabled: s.google != nil,
		})
	}

	if !store.ValidID(username) {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		fail()
		return
	}
	u, err := s.store.GetUser(r.Context(), username)
	if err != nil || u.PasswordHash == "" {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		fail()
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		fail()
		return
	}
	s.setSession(w, r, u)
	http.Redirect(w, r, localPath(next), http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleLogoutEverywhere bumps CredentialVersion, so every outstanding
// session — this one included — dies on its next request.
func (s *server) handleLogoutEverywhere(w http.ResponseWriter, r *http.Request, u store.User) {
	u.CredentialVersion++
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- credential management (session-only) ---

// handleSetPassword sets or changes the password. Changing requires the
// current one; setting a first password on a Google-only account only
// requires the session. All other sessions are logged out.
func (s *server) handleSetPassword(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if len(req.New) < minPasswordLen {
		http.Error(w, fmt.Sprintf("password must be at least %d characters", minPasswordLen), http.StatusBadRequest)
		return
	}
	if u.PasswordHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Current)) != nil {
			http.Error(w, "current password is wrong", http.StatusForbidden)
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		s.fail(w, err)
		return
	}
	u.PasswordHash = string(hash)
	u.CredentialVersion++
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	// Re-issue this browser's cookie at the new version; every other
	// session is now stale.
	s.setSession(w, r, u)
	w.WriteHeader(http.StatusNoContent)
}

// handleGoogleUnlink detaches the Google identity. Refused when it is
// the only Login left — that would lock the account.
func (s *server) handleGoogleUnlink(w http.ResponseWriter, r *http.Request, u store.User) {
	if u.GoogleSub == "" {
		http.Error(w, "no Google account is linked", http.StatusConflict)
		return
	}
	if u.PasswordHash == "" {
		http.Error(w, "set a password first — unlinking now would lock you out", http.StatusConflict)
		return
	}
	u.GoogleSub, u.GoogleEmail = "", ""
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- API key management (session-only) ---

// apiKeyView is an APIKey as its owner sees it; Key is set only in the
// minting response — the one time the plaintext exists.
type apiKeyView struct {
	store.APIKey
	Key string `json:"key,omitempty"`
}

func (s *server) handleListAPIKeys(w http.ResponseWriter, r *http.Request, u store.User) {
	keys, err := s.store.ListAPIKeys(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, keys)
}

func (s *server) handleMintAPIKey(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required — say which agent this key is for", http.StatusBadRequest)
		return
	}
	keyID, err := randomHex(8)
	if err != nil {
		s.fail(w, err)
		return
	}
	secret, err := randomHex(24)
	if err != nil {
		s.fail(w, err)
		return
	}
	k := store.APIKey{
		UserID:     u.ID,
		KeyID:      keyID,
		Name:       req.Name,
		SecretHash: secretHash(secret),
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.store.PutAPIKey(r.Context(), k); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, apiKeyView{
		APIKey: k,
		Key:    apiKeyPrefix + keyID + "_" + secret,
	})
}

func (s *server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request, u store.User) {
	keyID := r.PathValue("keyid")
	k, err := s.store.GetAPIKey(r.Context(), keyID)
	if err != nil || k.UserID != u.ID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.store.DeleteAPIKey(r.Context(), keyID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSON reads a small JSON body, writing the 400 itself on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
	if err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
	}
	return err
}
