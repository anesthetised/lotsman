package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// A user's link is the secret in the address of their page, /u/<token>. It
// lets whoever holds it download the user's configs and change their
// devices, so it is kept like a password: 256 random bits, only their
// SHA-256 in the database, and the token itself shown once, when issued.

// TokenLen is the length of a link token: 32 bytes in unpadded base64url.
const TokenLen = 43

// IssueLink gives the user a new link and revokes the one they had. The
// token it returns is not stored anywhere and cannot be shown again.
// ErrNotFound if there is no such user.
func (s *Store) IssueLink(user string) (string, error) {
	raw := make([]byte, 32)
	rand.Read(raw) // never fails: it crashes the program instead
	token := base64.RawURLEncoding.EncodeToString(raw)
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	userID, err := userID(tx, user)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`UPDATE user_links SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, userID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO user_links (user_id, token_hash, created_at) VALUES (?, ?, ?)`, userID, hashToken(token), now); err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// RevokeLink ends the user's link, if they have one. ErrNotFound if there
// is no such user.
func (s *Store) RevokeLink(user string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	userID, err := userID(tx, user)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE user_links SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveLink returns the user whose live link token is. ErrNotFound for
// anything else: a malformed, unknown or revoked token, or one of a user
// since removed.
func (s *Store) ResolveLink(token string) (string, error) {
	if !ValidToken(token) {
		return "", ErrNotFound
	}
	var name string
	err := s.db.QueryRow(`SELECT u.name FROM user_links l JOIN users u ON u.id = l.user_id
		WHERE l.token_hash = ? AND l.revoked_at IS NULL`, hashToken(token)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// ValidToken reports whether token has the form IssueLink gives tokens.
func ValidToken(token string) bool {
	if len(token) != TokenLen {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func userID(tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM users WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
