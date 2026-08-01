package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// pairingCodeTTL is how long a displayed pairing code stays valid.
const pairingCodeTTL = 5 * time.Minute

// AuthService issues and validates client tokens for the hosted POS.
// Pairing: the dashboard shows a one-time 6-digit code; the POS page
// exchanges it for a long-lived random token. Only the token's SHA-256 is
// stored.
type AuthService struct {
	db *sql.DB

	mu            sync.Mutex
	pairingCode   string
	pairingExpiry time.Time
}

func NewAuthService(db *sql.DB) *AuthService { return &AuthService{db: db} }

// GeneratePairingCode invalidates any previous code and returns a new one.
func (a *AuthService) GeneratePairingCode() (string, time.Time) {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	code := fmt.Sprintf("%06d", n.Int64())
	a.mu.Lock()
	a.pairingCode = code
	a.pairingExpiry = time.Now().Add(pairingCodeTTL)
	a.mu.Unlock()
	return code, a.pairingExpiry
}

var ErrPairingRejected = errors.New("invalid or expired pairing code")

// Pair exchanges a valid one-time code for a new client token. The clear
// token is returned exactly once.
func (a *AuthService) Pair(code, origin, label string) (string, error) {
	a.mu.Lock()
	valid := a.pairingCode != "" && time.Now().Before(a.pairingExpiry) &&
		subtle.ConstantTimeCompare([]byte(code), []byte(a.pairingCode)) == 1
	if valid {
		a.pairingCode = "" // one-time use
	}
	a.mu.Unlock()
	if !valid {
		return "", ErrPairingRejected
	}

	raw := make([]byte, 32)
	rand.Read(raw)
	token := "pat_" + hex.EncodeToString(raw)
	id := hex.EncodeToString(raw[:8])
	if label == "" {
		label = "POS client"
	}
	_, err := a.db.Exec(`INSERT INTO client_tokens (id, token_hash, label, allowed_origin, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, hashToken(token), label, origin, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return token, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ValidateToken reports whether the bearer token is active, and updates its
// last-used timestamp.
func (a *AuthService) ValidateToken(token string) bool {
	if token == "" {
		return false
	}
	var id string
	err := a.db.QueryRow(`SELECT id FROM client_tokens
		WHERE token_hash = ? AND revoked_at IS NULL`, hashToken(token)).Scan(&id)
	if err != nil {
		return false
	}
	a.db.Exec(`UPDATE client_tokens SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	return true
}

// TokenInfo is the dashboard view of an issued token (never the token).
type TokenInfo struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	AllowedOrigin string `json:"allowedOrigin"`
	CreatedAt     string `json:"createdAt"`
	LastUsedAt    string `json:"lastUsedAt,omitempty"`
	Revoked       bool   `json:"revoked"`
}

func (a *AuthService) ListTokens() ([]TokenInfo, error) {
	rows, err := a.db.Query(`SELECT id, COALESCE(label, ''), allowed_origin, created_at,
		COALESCE(last_used_at, ''), revoked_at IS NOT NULL
		FROM client_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Label, &t.AllowedOrigin, &t.CreatedAt, &t.LastUsedAt, &t.Revoked); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *AuthService) RevokeToken(id string) error {
	res, err := a.db.Exec(`UPDATE client_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("token not found or already revoked")
	}
	return nil
}

// HasActiveToken reports whether any unrevoked client token exists (shown on
// the dashboard's pairing panel).
func (a *AuthService) HasActiveToken() bool {
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM client_tokens WHERE revoked_at IS NULL`).Scan(&n)
	return n > 0
}
