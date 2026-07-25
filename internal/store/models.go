// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// Application roles. An account has exactly one.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Account is a login identity. It never carries a plaintext secret, and
// PasswordHash is not serialised to clients.
type Account struct {
	ID          int64
	Email       string
	Role        string
	DisplayName string
	Timezone    string
	AdminNotes  string
	IsActive    bool
	// Activated is false while an invited account has no password yet.
	Activated   bool
	ActivatedAt time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// passwordHash stays unexported so it cannot leak through a struct literal
	// copy into a response body. Use VerifyPassword.
	passwordHash string
}

// IsAdmin reports whether the account holds the admin application role.
func (a *Account) IsAdmin() bool { return a.Role == RoleAdmin }

// Game is a single match. Games are deactivated, never deleted.
type Game struct {
	ID        int64
	Name      string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Membership links a user account to a game. Admin accounts can never appear
// here: the database enforces it through a composite foreign key.
//
// It describes a human seat. game_player also holds agent seats, which have no
// account and are played by the engine; those are not memberships and have no
// HTTP surface yet.
type Membership struct {
	// ID is the seat's row id, which is the engine's player_id. It is the only
	// identity that crosses into the engine domain — nothing under the engine
	// references an account.
	ID          int64
	GameID      int64
	AccountID   int64
	Email       string
	DisplayName string
	GameName    string
	IsGM        bool
	IsActive    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AgentSeat is a seat played by the engine rather than by a person. It has no
// account: that is what makes "an agent cannot sign in" a fact about the
// schema rather than a property of some unusable credential.
//
// AgentKey names the implementation that plays it. Whether this build has that
// implementation is a question only internal/engine can answer, so the store
// reads and writes the key without judging it.
type AgentSeat struct {
	// ID is the seat's row id, which is the engine's player_id — the same
	// identity space human seats draw from, because the engine must not care
	// which kind it is dealing with.
	ID        int64
	GameID    int64
	GameName  string
	AgentKey  string
	AgentName string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Session is a server-side authenticated session. The cookie value itself is
// never stored; only its fingerprint is.
type Session struct {
	ID         int64
	AccountID  int64
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	UserAgent  string
	RemoteIP   string

	// ImpersonatedAccountID is non-zero while an administrator is acting as
	// another account.
	ImpersonatedAccountID int64
}

// IsImpersonating reports whether this session is currently acting as another
// account.
func (s *Session) IsImpersonating() bool { return s.ImpersonatedAccountID != 0 }

// ActivationLink is a freshly minted magic link. Token is plaintext and is
// returned exactly once, for the administrator to deliver out of band; it is
// never persisted or logged.
type ActivationLink struct {
	AccountID int64
	Token     string
	ExpiresAt time.Time
}

// ActivationTTL is how long a magic activation link remains redeemable.
const ActivationTTL = 48 * time.Hour

// Page describes a slice of a list endpoint's results.
type Page struct {
	Number  int
	Size    int
	Total   int
	Entries int
}

// Offset returns the SQL offset for the page.
func (p Page) Offset() int { return (p.Number - 1) * p.Size }

// TotalPages returns the number of pages covering Total rows, at least 1.
func (p Page) TotalPages() int {
	if p.Size <= 0 || p.Total <= 0 {
		return 1
	}
	pages := p.Total / p.Size
	if p.Total%p.Size != 0 {
		pages++
	}
	return pages
}

// NormalizeEmail lowercases and trims an address so lookups and the unique
// index agree. It returns an error if the result is not a plausible address,
// which keeps the database CHECK constraint from being the first line of
// defence.
func NormalizeEmail(email string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return "", fmt.Errorf("email is required")
	}
	if len(normalized) > 254 {
		return "", fmt.Errorf("email must be at most 254 bytes")
	}
	addr, err := mail.ParseAddress(normalized)
	if err != nil || addr.Address != normalized {
		return "", fmt.Errorf("email is not a valid address")
	}
	return normalized, nil
}

// timeFormat is the on-disk timestamp format: RFC 3339 in UTC, which sorts
// lexicographically in chronological order.
const timeFormat = "2006-01-02T15:04:05.000Z"

// Now returns the current UTC time truncated to the precision timestamps are
// stored with.
//
// Truncating here means a timestamp echoed straight back in a response is
// identical to the one a later read returns, instead of differing in digits
// that the storage format drops.
func Now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// formatTime renders t for storage.
func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

// parseTime reads a stored timestamp. An empty string yields the zero time,
// which is how nullable columns such as activated_at are represented.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t, nil
}

// A game's PCG seed is two uint64 words, and SQLite's INTEGER is a *signed*
// 64-bit value. There is no unsigned column type to reach for and no wider one
// to hide in, so the two words are stored as the signed integers with the same
// bits — which means a seed word at or above 2^63 is on disk as a negative
// number.
//
// That is not corruption and must not be "fixed". Go defines a conversion
// between integer types of the same size as a reinterpretation of the bits, so
// the round trip through int64 and back is exact for every value a uint64 can
// hold, including the whole upper half. Clamping, or widening to TEXT to keep
// the number looking positive, would each cost more than the confusion they
// save: a seed that does not round-trip exactly makes a game unreplayable,
// which is the one property internal/engine exists to guarantee.
//
// These two functions are the only place the conversion happens. Nothing in a
// handler and nothing in the engine should ever cast a seed itself — that is
// how one path acquires a subtly different rule from another.

// formatSeed renders a seed word for storage.
func formatSeed(word uint64) int64 { return int64(word) }

// parseSeed reads a stored seed word.
func parseSeed(stored int64) uint64 { return uint64(stored) }

// boolToInt converts a Go bool to SQLite's 0/1 representation.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
