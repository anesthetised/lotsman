package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	// authPrefix is where the panel's own sign-in endpoints live. They are
	// not part of lotsman.v1: the daemon never sees operators.
	authPrefix = "/auth"
	// sessionCookie carries the session ID. Browsers accept a __Host-
	// cookie only with Secure, Path=/ and no Domain, so a sibling
	// subdomain cannot plant or overwrite it.
	sessionCookie = "__Host-lotsman-session"
	// sessionIdle ends a session unused for that long; sessionMax ends any
	// session that old.
	sessionIdle = time.Hour
	sessionMax  = 12 * time.Hour
	// seenEvery bounds how often requests write a session's last use.
	seenEvery = time.Minute
	// EnrollmentTTL is how long an enrollment link stays valid.
	EnrollmentTTL = 15 * time.Minute
	// ceremonyTTL is how long the browser has between a ceremony's begin
	// and finish, the passkey prompt included.
	ceremonyTTL = 5 * time.Minute
	// maxCeremonies bounds the ceremonies open at once, all clients
	// together.
	maxCeremonies = 1024
	// Each client gets signInBurst ceremonies, sign-in and enrollment
	// alike, refilled at one per signInEvery.
	signInBurst  = 10
	signInEvery  = 30 * time.Second
	maxNameRunes = 64
)

var (
	errNoSession      = errors.New("not signed in")
	errSessionExpired = errors.New("the session has expired; sign in again")
	errNoUserLinks    = errors.New("user links are not available yet")
)

// OperatorAuth signs operators in with passkeys (WebAuthn) and keeps their
// sessions in web.db. It is the panel's Authenticator, and it serves the
// sign-in endpoints under /auth/. Every operator is equal: each passkey is
// an operator of its own, enrolled with a one-time link.
type OperatorAuth struct {
	db        *sql.DB
	rp        *webauthn.WebAuthn
	now       func() time.Time
	limit     *limiter
	pending   *ceremonies
	log       *slog.Logger
	mux       *http.ServeMux
	enrollURL func(token string) string
}

// NewOperatorAuth binds passkeys to cfg's public URL: its host is the RP ID
// and its origin the only one accepted.
func NewOperatorAuth(cfg *Config, db *sql.DB, log *slog.Logger) (*OperatorAuth, error) {
	rp, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.rpID,
		RPDisplayName: "Lotsman",
		RPOrigins:     []string{cfg.origin},
		// Any authenticator will do: no attestation, so no vendor list.
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			// A discoverable credential, so sign-in needs no user name, and
			// a PIN or biometric on every use.
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		// What the browser is told; ceremonies enforces it.
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: ceremonyTTL},
			Registration: webauthn.TimeoutConfig{Timeout: ceremonyTTL},
		},
	})
	if err != nil {
		return nil, err
	}
	a := &OperatorAuth{
		db: db, rp: rp, now: time.Now, log: log, enrollURL: cfg.EnrollURL,
		limit:   newLimiter(signInBurst, signInEvery),
		pending: newCeremonies(maxCeremonies),
		mux:     http.NewServeMux(),
	}
	// Every endpoint is a POST with a JSON body, so the origin check
	// applies to all of them; see sameOrigin.
	a.mux.HandleFunc("POST /auth/session", a.whoami)
	a.mux.HandleFunc("POST /auth/logout", a.logout)
	a.mux.HandleFunc("POST /auth/login/begin", a.loginBegin)
	a.mux.HandleFunc("POST /auth/login/finish", a.loginFinish)
	a.mux.HandleFunc("POST /auth/enroll/begin", a.enrollBegin)
	a.mux.HandleFunc("POST /auth/enroll/finish", a.enrollFinish)
	a.mux.HandleFunc("POST /auth/passkeys", a.operatorOnly(a.listPasskeys))
	a.mux.HandleFunc("POST /auth/passkeys/revoke", a.operatorOnly(a.revokePasskey))
	a.mux.HandleFunc("POST /auth/enrollments", a.operatorOnly(a.issueEnrollment))
	return a, nil
}

func (a *OperatorAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.mux.ServeHTTP(w, r)
}

// Operator is nil when r carries a live operator session.
func (a *OperatorAuth) Operator(r *http.Request) error {
	_, err := a.session(r)
	return err
}

// User refuses everyone until user links (#134) land.
func (a *OperatorAuth) User(*http.Request) (string, error) { return "", errNoUserLinks }

// passkey is an operator: the webauthn.User of one credential.
type passkey struct {
	id     int64
	handle []byte
	name   string
	cred   webauthn.Credential
}

func (p *passkey) WebAuthnID() []byte          { return p.handle }
func (p *passkey) WebAuthnName() string        { return p.name }
func (p *passkey) WebAuthnDisplayName() string { return p.name }
func (p *passkey) WebAuthnCredentials() []webauthn.Credential {
	if p.cred.ID == nil {
		return nil // not registered yet
	}
	return []webauthn.Credential{p.cred}
}

type operatorSession struct {
	idHash  []byte
	passkey int64
	name    string
}

// session is r's live session. A request refreshes its last use, at most
// once per seenEvery; an expired one is deleted.
func (a *OperatorAuth) session(r *http.Request) (operatorSession, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return operatorSession{}, errNoSession
	}
	s := operatorSession{idHash: hashSecret(c.Value)}
	var created, seen int64
	err = a.db.QueryRowContext(r.Context(), `SELECT s.passkey_id, p.name, s.created_at, s.seen_at
		FROM sessions s JOIN passkeys p ON p.id = s.passkey_id WHERE s.id_hash = ?`, s.idHash).
		Scan(&s.passkey, &s.name, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return s, errNoSession
	}
	if err != nil {
		a.log.Warn("session lookup failed", "error", err)
		return s, errors.New("sessions are unavailable")
	}
	now := a.now().Unix()
	if now-created >= seconds(sessionMax) || now-seen >= seconds(sessionIdle) {
		a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE id_hash = ?`, s.idHash)
		return s, errSessionExpired
	}
	if now-seen >= seconds(seenEvery) {
		a.db.ExecContext(r.Context(), `UPDATE sessions SET seen_at = ? WHERE id_hash = ?`, now, s.idHash)
	}
	return s, nil
}

// startSession signs the browser in as the operator of passkey id, ending
// the session it had, if any.
func (a *OperatorAuth) startSession(w http.ResponseWriter, r *http.Request, id int64) error {
	a.endSession(r)
	now := a.now().Unix()
	// Forget the sessions nobody can use any more, while at it.
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE created_at <= ? OR seen_at <= ?`,
		now-seconds(sessionMax), now-seconds(sessionIdle)); err != nil {
		return err
	}
	secret := newSecret()
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO sessions (id_hash, passkey_id, created_at, seen_at) VALUES (?, ?, ?, ?)`,
		hashSecret(secret), id, now, now); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: secret, Path: "/", MaxAge: int(seconds(sessionMax)),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (a *OperatorAuth) endSession(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE id_hash = ?`, hashSecret(c.Value))
	}
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (a *OperatorAuth) operatorOnly(h func(http.ResponseWriter, *http.Request, operatorSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		s, err := a.session(r)
		if err != nil {
			fail(w, http.StatusUnauthorized, err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r, s)
	}
}

// whoami answers whether r is signed in, and as which passkey. Signed out
// is an answer, not an error, so a page asking logs nothing.
func (a *OperatorAuth) whoami(w http.ResponseWriter, r *http.Request) {
	if _, ok := readBody(w, r); !ok {
		return
	}
	s, err := a.session(r)
	if err != nil {
		writeJSON(w, map[string]any{"signedIn": false})
		return
	}
	writeJSON(w, map[string]any{"signedIn": true, "name": s.name})
}

func (a *OperatorAuth) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := readBody(w, r); !ok {
		return
	}
	a.endSession(r)
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// begin admits one ceremony from r's client.
func (a *OperatorAuth) begin(w http.ResponseWriter, r *http.Request) bool {
	if !a.limit.allow(ClientAddr(r), a.now()) {
		w.Header().Set("Retry-After", "30")
		fail(w, http.StatusTooManyRequests, "too many sign-in attempts; wait a minute")
		return false
	}
	return true
}

func (a *OperatorAuth) open(w http.ResponseWriter, c ceremony, options any) {
	c.expires = a.now().Add(ceremonyTTL)
	if !a.pending.put(c, a.now()) {
		fail(w, http.StatusServiceUnavailable, "too many sign-ins in progress; try again later")
		return
	}
	writeJSON(w, options)
}

func (a *OperatorAuth) loginBegin(w http.ResponseWriter, r *http.Request) {
	if _, ok := readBody(w, r); !ok || !a.begin(w, r) {
		return
	}
	options, session, err := a.rp.BeginDiscoverableLogin()
	if err != nil {
		a.internal(w, "begin sign-in", err)
		return
	}
	a.open(w, ceremony{kind: signIn, session: *session}, options)
}

func (a *OperatorAuth) loginFinish(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		fail(w, http.StatusBadRequest, "malformed credential")
		return
	}
	c, ok := a.pending.take(parsed.Response.CollectedClientData.Challenge, signIn, a.now())
	if !ok {
		fail(w, http.StatusBadRequest, "unknown or expired sign-in; start again")
		return
	}
	var p *passkey
	_, cred, err := a.rp.ValidatePasskeyLogin(func(rawID, _ []byte) (webauthn.User, error) {
		// The library checks that handle is p's.
		p, err = a.passkey(r, rawID)
		return p, err
	}, c.session, parsed)
	if err == nil && cred.Authenticator.CloneWarning {
		// The signature counter went back: the key may have been copied.
		err = errors.New("signature counter went back")
		a.log.Warn("passkey refused: it may have been cloned", "passkey", p.name, "client", ClientAddr(r))
	}
	if err != nil {
		a.log.Info("sign-in refused", "client", ClientAddr(r), "error", detail(err))
		fail(w, http.StatusUnauthorized, "the passkey was not accepted")
		return
	}
	stored, _ := json.Marshal(cred)
	if _, err := a.db.ExecContext(r.Context(), `UPDATE passkeys SET credential = ?, used_at = ? WHERE id = ?`,
		stored, a.now().Unix(), p.id); err != nil {
		a.internal(w, "record sign-in", err)
		return
	}
	if err := a.startSession(w, r, p.id); err != nil {
		a.internal(w, "start session", err)
		return
	}
	a.log.Info("operator signed in", "passkey", p.name, "client", ClientAddr(r))
	writeJSON(w, map[string]any{"name": p.name})
}

func (a *OperatorAuth) passkey(r *http.Request, credentialID []byte) (*passkey, error) {
	p := &passkey{}
	var cred []byte
	err := a.db.QueryRowContext(r.Context(), `SELECT id, user_handle, name, credential FROM passkeys WHERE credential_id = ?`,
		credentialID).Scan(&p.id, &p.handle, &p.name, &cred)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("no such passkey; it may have been revoked")
	}
	if err != nil {
		return nil, err
	}
	return p, json.Unmarshal(cred, &p.cred)
}

type enrollmentState int

const (
	enrollmentValid enrollmentState = iota
	enrollmentUnknown
	enrollmentUsed
	enrollmentExpired
)

func (a *OperatorAuth) enrollment(r *http.Request, hash []byte) (enrollmentState, error) {
	var expires int64
	var used sql.NullInt64
	err := a.db.QueryRowContext(r.Context(), `SELECT expires_at, used_at FROM enrollments WHERE token_hash = ?`, hash).
		Scan(&expires, &used)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return enrollmentUnknown, nil
	case err != nil:
		return 0, err
	case used.Valid:
		return enrollmentUsed, nil
	case a.now().Unix() >= expires:
		return enrollmentExpired, nil
	}
	return enrollmentValid, nil
}

func (a *OperatorAuth) enrollBegin(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok || !a.begin(w, r) {
		return
	}
	var req struct{ Token, Name string }
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, http.StatusBadRequest, "malformed request")
		return
	}
	name, err := passkeyName(req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	hash := hashSecret(req.Token)
	state, err := a.enrollment(r, hash)
	if err != nil {
		a.internal(w, "look up enrollment", err)
		return
	}
	switch state {
	case enrollmentUnknown:
		fail(w, http.StatusNotFound, "unknown enrollment link")
		return
	case enrollmentUsed:
		fail(w, http.StatusGone, "this enrollment link has been used")
		return
	case enrollmentExpired:
		fail(w, http.StatusGone, "this enrollment link has expired")
		return
	}
	operator := &passkey{handle: randomBytes(32), name: name}
	options, session, err := a.rp.BeginRegistration(operator)
	if err != nil {
		a.internal(w, "begin enrollment", err)
		return
	}
	a.open(w, ceremony{kind: enroll, session: *session, enrollment: hash, operator: operator}, options)
}

func (a *OperatorAuth) enrollFinish(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		fail(w, http.StatusBadRequest, "malformed credential")
		return
	}
	c, ok := a.pending.take(parsed.Response.CollectedClientData.Challenge, enroll, a.now())
	if !ok {
		fail(w, http.StatusBadRequest, "unknown or expired enrollment; start again")
		return
	}
	cred, err := a.rp.CreateCredential(c.operator, c.session, parsed)
	if err != nil {
		a.log.Info("enrollment refused", "client", ClientAddr(r), "error", detail(err))
		fail(w, http.StatusBadRequest, "the passkey was not accepted")
		return
	}
	id, err := a.addPasskey(r, c, cred)
	if errors.Is(err, errEnrollmentGone) {
		fail(w, http.StatusGone, err.Error())
		return
	}
	if err != nil {
		a.internal(w, "store passkey", err)
		return
	}
	if err := a.startSession(w, r, id); err != nil {
		a.internal(w, "start session", err)
		return
	}
	a.log.Info("operator enrolled", "passkey", c.operator.name, "client", ClientAddr(r))
	writeJSON(w, map[string]any{"name": c.operator.name})
}

var errEnrollmentGone = errors.New("this enrollment link has been used or has expired")

// addPasskey uses up c's enrollment and stores its new passkey, both or
// neither: of two ceremonies begun with one link, only the first to finish
// gets in.
func (a *OperatorAuth) addPasskey(r *http.Request, c ceremony, cred *webauthn.Credential) (int64, error) {
	stored, err := json.Marshal(cred)
	if err != nil {
		return 0, err
	}
	now := a.now().Unix()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE enrollments SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		now, c.enrollment, now)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errEnrollmentGone
	}
	res, err = tx.Exec(`INSERT INTO passkeys (credential_id, user_handle, credential, name, created_at) VALUES (?, ?, ?, ?, ?)`,
		cred.ID, c.operator.handle, stored, c.operator.name, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (a *OperatorAuth) listPasskeys(w http.ResponseWriter, r *http.Request, s operatorSession) {
	type item struct {
		ID        int64      `json:"id"`
		Name      string     `json:"name"`
		CreatedAt time.Time  `json:"createdAt"`
		UsedAt    *time.Time `json:"usedAt,omitempty"`
		Current   bool       `json:"current"`
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, name, created_at, used_at FROM passkeys ORDER BY created_at, id`)
	if err != nil {
		a.internal(w, "list passkeys", err)
		return
	}
	defer rows.Close()
	list := []item{}
	for rows.Next() {
		var it item
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&it.ID, &it.Name, &created, &used); err != nil {
			a.internal(w, "list passkeys", err)
			return
		}
		it.CreatedAt, it.Current = time.Unix(created, 0).UTC(), it.ID == s.passkey
		if used.Valid {
			t := time.Unix(used.Int64, 0).UTC()
			it.UsedAt = &t
		}
		list = append(list, it)
	}
	if err := rows.Err(); err != nil {
		a.internal(w, "list passkeys", err)
		return
	}
	writeJSON(w, map[string]any{"passkeys": list})
}

// revokePasskey deletes a passkey, and with it every session it signed
// in. Revoking the last one locks the panel until `lotsman web enroll`.
func (a *OperatorAuth) revokePasskey(w http.ResponseWriter, r *http.Request, s operatorSession) {
	var req struct{ ID int64 }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "malformed request")
		return
	}
	var name string
	err := a.db.QueryRowContext(r.Context(), `DELETE FROM passkeys WHERE id = ? RETURNING name`, req.ID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "no such passkey")
		return
	}
	if err != nil {
		a.internal(w, "revoke passkey", err)
		return
	}
	a.log.Info("passkey revoked", "passkey", name, "by", s.name, "client", ClientAddr(r))
	self := req.ID == s.passkey
	if self {
		clearSessionCookie(w)
	}
	writeJSON(w, map[string]any{"signedOut": self})
}

func (a *OperatorAuth) issueEnrollment(w http.ResponseWriter, r *http.Request, s operatorSession) {
	token, expires, err := IssueEnrollment(r.Context(), a.db, a.now())
	if err != nil {
		a.internal(w, "issue enrollment", err)
		return
	}
	a.log.Info("enrollment link issued", "by", s.name, "expires", expires)
	writeJSON(w, map[string]any{"url": a.enrollURL(token), "expiresAt": expires.UTC()})
}

// IssueEnrollment stores a new one-time enrollment token, valid for
// EnrollmentTTL from now, and returns it. Only its hash is kept.
func IssueEnrollment(ctx context.Context, db *sql.DB, now time.Time) (token string, expires time.Time, err error) {
	// Forget the links nobody can use any more, while at it.
	if _, err := db.ExecContext(ctx, `DELETE FROM enrollments WHERE expires_at <= ?`, now.Unix()); err != nil {
		return "", time.Time{}, err
	}
	token, expires = newSecret(), now.Add(EnrollmentTTL).Truncate(time.Second)
	if _, err := db.ExecContext(ctx, `INSERT INTO enrollments (token_hash, expires_at) VALUES (?, ?)`,
		hashSecret(token), expires.Unix()); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func (a *OperatorAuth) internal(w http.ResponseWriter, what string, err error) {
	a.log.Error(what, "error", err)
	fail(w, http.StatusInternalServerError, "internal error")
}

// readBody reads a request body of at most maxRequestBytes. It must be
// JSON (or empty with a JSON content type): no HTML form can send that, so
// a forged cross-site POST is refused here even if the origin check and
// SameSite cookies were somehow bypassed.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "want application/json")
		return nil, false
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		fail(w, http.StatusBadRequest, "request body: "+err.Error())
		return nil, false
	}
	return body, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// detail is the library's explanation of a refused ceremony.
func detail(err error) string {
	var perr *protocol.Error
	if errors.As(err, &perr) && perr.DevInfo != "" {
		return perr.Details + ": " + perr.DevInfo
	}
	return err.Error()
}

// passkeyName checks the name an operator gives a passkey; the browser
// shows it next to the panel's name in its passkey list.
func passkeyName(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("name the passkey, such as \"laptop\"")
	case utf8.RuneCountInString(s) > maxNameRunes:
		return "", errors.New("the name is too long")
	case strings.ContainsFunc(s, unicode.IsControl), !utf8.ValidString(s):
		return "", errors.New("the name has control characters")
	}
	return s, nil
}

// newSecret is 256 random bits, base64url: session IDs and enrollment
// tokens.
func newSecret() string { return base64.RawURLEncoding.EncodeToString(randomBytes(32)) }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b) // never fails
	return b
}

func hashSecret(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }
