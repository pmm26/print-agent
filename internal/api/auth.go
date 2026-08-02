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
	"net/http"
	"strings"
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

	mu              sync.Mutex
	pairingCode     string
	pairingExpiry   time.Time
	pairingFailures int
	lastPersisted   map[string]time.Time
	eventTickets    map[string]time.Time
}

func NewAuthService(db *sql.DB) *AuthService {
	return &AuthService{db: db, lastPersisted: make(map[string]time.Time), eventTickets: make(map[string]time.Time)}
}

// GeneratePairingCode invalidates any previous code and returns a new one.
func (a *AuthService) GeneratePairingCode() (string, time.Time, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate pairing code: %w", err)
	}
	code := fmt.Sprintf("%06d", n.Int64())
	a.mu.Lock()
	a.pairingCode = code
	a.pairingExpiry = time.Now().Add(pairingCodeTTL)
	a.pairingFailures = 0
	expires := a.pairingExpiry
	a.mu.Unlock()
	return code, expires, nil
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
		a.pairingFailures = 0
	} else if a.pairingCode != "" {
		a.pairingFailures++
		if a.pairingFailures >= 5 {
			a.pairingCode = ""
		}
	}
	a.mu.Unlock()
	if !valid {
		return "", ErrPairingRejected
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate client token: %w", err)
	}
	token := "pat_" + hex.EncodeToString(raw)
	id := hex.EncodeToString(raw[:8])
	if label == "" {
		label = "POS client"
	}
	tx, err := a.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO api_credentials
		(id, token_hash, credential_type, label, allowed_origin, created_at)
		VALUES (?, ?, 'pos', ?, ?, ?)`, id, hashToken(token), label, origin, now); err != nil {
		return "", err
	}
	for _, scope := range []string{"jobs:submit", "jobs:read", "status:read"} {
		if _, err = tx.Exec(`INSERT INTO credential_scopes (credential_id, scope) VALUES (?, ?)`, id, scope); err != nil {
			return "", err
		}
	}
	err = tx.Commit()
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
func (a *AuthService) ValidateToken(token, origin string) bool {
	ok, _ := a.validateToken(token, origin)
	return ok
}

func (a *AuthService) validateToken(token, origin string) (bool, error) {
	return a.validateTokenScope(token, origin, "")
}

func (a *AuthService) validateTokenScope(token, origin, scope string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var id string
	query := `SELECT c.id FROM api_credentials c WHERE c.token_hash = ? AND c.revoked_at IS NULL
		AND (c.allowed_origin IS NULL OR c.allowed_origin = ?)`
	args := []any{hashToken(token), origin}
	if scope != "" {
		query += ` AND EXISTS (SELECT 1 FROM credential_scopes s WHERE s.credential_id = c.id AND s.scope IN (?, 'admin'))`
		args = append(args, scope)
	}
	err := a.db.QueryRow(query, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now()
	a.mu.Lock()
	last := a.lastPersisted[id]
	a.mu.Unlock()
	if now.Sub(last) >= 5*time.Minute {
		if _, err := a.db.Exec(`UPDATE api_credentials SET last_used_at = ? WHERE id = ?`,
			now.UTC().Format(time.RFC3339Nano), id); err == nil {
			a.mu.Lock()
			a.lastPersisted[id] = now
			a.mu.Unlock()
		} else {
			return true, err
		}
	}
	return true, nil
}

// IssueEventTicket returns a one-use, short-lived browser ticket. It lets the
// same-origin dashboard open a WebSocket without placing a long-lived bearer
// token in a URL.
func (a *AuthService) IssueEventTicket() (string, time.Time, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	ticket := "wst_" + hex.EncodeToString(raw)
	expires := time.Now().UTC().Add(30 * time.Second)
	a.mu.Lock()
	for key, expiry := range a.eventTickets {
		if time.Now().After(expiry) {
			delete(a.eventTickets, key)
		}
	}
	a.eventTickets[hashToken(ticket)] = expires
	a.mu.Unlock()
	return ticket, expires, nil
}

func (a *AuthService) ConsumeEventTicket(ticket string) bool {
	if ticket == "" {
		return false
	}
	key := hashToken(ticket)
	a.mu.Lock()
	expires, ok := a.eventTickets[key]
	delete(a.eventTickets, key)
	a.mu.Unlock()
	return ok && time.Now().Before(expires)
}

func (a *AuthService) AuthorizeEventsRequest(r *http.Request) bool {
	if a.ConsumeEventTicket(r.URL.Query().Get("ticket")) {
		return true
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	ok, _ := a.validateTokenScope(token, r.Header.Get("Origin"), "events:read")
	return ok
}

// CreateEventCredential returns the clear token once and stores only its hash.
func (a *AuthService) CreateEventCredential(label string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "pae_" + hex.EncodeToString(raw)
	id := hex.EncodeToString(raw[:8])
	if label == "" {
		label = "Event consumer"
	}
	tx, err := a.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO api_credentials
		(id, token_hash, credential_type, label, created_at) VALUES (?, ?, 'events', ?, ?)`,
		id, hashToken(token), label, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO credential_scopes (credential_id, scope) VALUES (?, 'events:read')`, id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

// TokenInfo is the dashboard view of an issued token (never the token).
type TokenInfo struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Label         string `json:"label"`
	AllowedOrigin string `json:"allowedOrigin"`
	CreatedAt     string `json:"createdAt"`
	LastUsedAt    string `json:"lastUsedAt,omitempty"`
	Revoked       bool   `json:"revoked"`
}

func (a *AuthService) ListTokens() ([]TokenInfo, error) {
	rows, err := a.db.Query(`SELECT id, credential_type, COALESCE(label, ''), COALESCE(allowed_origin, ''), created_at,
		COALESCE(last_used_at, ''), revoked_at IS NOT NULL
		FROM api_credentials ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Type, &t.Label, &t.AllowedOrigin, &t.CreatedAt, &t.LastUsedAt, &t.Revoked); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *AuthService) RevokeToken(id string) error {
	res, err := a.db.Exec(`UPDATE api_credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("token not found or already revoked")
	}
	return nil
}

// HasActiveToken reports whether an unrevoked POS credential exists (shown on
// the dashboard's pairing panel). Event-consumer credentials do not imply
// that a POS installation has paired.
func (a *AuthService) HasActiveToken() bool {
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM api_credentials WHERE credential_type = 'pos' AND revoked_at IS NULL`).Scan(&n)
	return n > 0
}
