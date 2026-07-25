// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"strconv"
	"time"

	"github.com/mdhender/ecv8-api/internal/engine"
	"github.com/mdhender/ecv8-api/internal/store"
)

// The types below are the only shapes the API emits. Nothing in this file
// carries a password hash, an activation-token hash, a session token, a SQL
// fragment, or a filesystem path; store models are never serialised directly,
// so a new database column cannot leak by accident.

// envelope wraps every successful response so the client parses one shape.
type envelope struct {
	Data any   `json:"data"`
	Meta *meta `json:"meta,omitempty"`
}

// meta carries pagination for list responses.
type meta struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// pageMeta converts a store page into its wire form.
func pageMeta(p store.Page) *meta {
	return &meta{
		Page:       p.Number,
		PerPage:    p.Size,
		Total:      p.Total,
		TotalPages: p.TotalPages(),
	}
}

// accountView is an account as its owner sees it.
type accountView struct {
	ID          int64      `json:"id"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	DisplayName string     `json:"display_name"`
	Timezone    string     `json:"timezone"`
	IsActive    bool       `json:"is_active"`
	Activated   bool       `json:"activated"`
	ActivatedAt *time.Time `json:"activated_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// newAccountView renders an account for its owner.
func newAccountView(a *store.Account) accountView {
	view := accountView{
		ID:          a.ID,
		Email:       a.Email,
		Role:        a.Role,
		DisplayName: a.DisplayName,
		Timezone:    a.Timezone,
		IsActive:    a.IsActive,
		Activated:   a.Activated,
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.UpdatedAt,
	}
	if !a.ActivatedAt.IsZero() {
		at := a.ActivatedAt
		view.ActivatedAt = &at
	}
	return view
}

// adminAccountView adds the fields only an administrator may see.
type adminAccountView struct {
	accountView
	AdminNotes string `json:"admin_notes"`
	// ActivationPending reports whether an unredeemed link is outstanding.
	ActivationPending bool `json:"activation_pending"`
	// ActivationExpiresAt is when that link stops working.
	ActivationExpiresAt *time.Time `json:"activation_expires_at"`
	// ActiveSessions is how many live sessions revocation would end.
	ActiveSessions int `json:"active_sessions"`
}

// newAdminAccountView renders an account for the admin UI.
func newAdminAccountView(a *store.Account, pendingAt *time.Time, sessions int) adminAccountView {
	return adminAccountView{
		accountView:         newAccountView(a),
		AdminNotes:          a.AdminNotes,
		ActivationPending:   pendingAt != nil,
		ActivationExpiresAt: pendingAt,
		ActiveSessions:      sessions,
	}
}

// sessionView describes the current session to the client.
//
// Account is who the request acts as. Impersonator is the real administrator
// when impersonation is active, which is what lets the UI show an unmistakable
// banner and an exit control.
type sessionView struct {
	Authenticated bool         `json:"authenticated"`
	Account       accountView  `json:"account"`
	IsAdmin       bool         `json:"is_admin"`
	Impersonating bool         `json:"impersonating"`
	Impersonator  *accountView `json:"impersonator"`
	ExpiresAt     time.Time    `json:"expires_at"`
}

// newSessionView renders the current identity.
func newSessionView(id *identity) sessionView {
	view := sessionView{
		Authenticated: true,
		Account:       newAccountView(id.Effective),
		IsAdmin:       id.HasAdminRights(),
		Impersonating: id.IsImpersonating(),
		ExpiresAt:     id.Session.ExpiresAt,
	}
	if id.IsImpersonating() {
		actor := newAccountView(id.Actor)
		view.Impersonator = &actor
	}
	return view
}

// activationLinkView is a freshly minted magic link.
//
// The URL contains the plaintext token and is returned exactly once, because
// only its hash is stored. The application does not send email, so this is the
// administrator's only copy.
type activationLinkView struct {
	AccountID int64     `json:"account_id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// gameView is a game.
type gameView struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// newGameView renders a game.
func newGameView(g *store.Game) gameView {
	return gameView{
		ID:        g.ID,
		Name:      g.Name,
		IsActive:  g.IsActive,
		CreatedAt: g.CreatedAt,
		UpdatedAt: g.UpdatedAt,
	}
}

// membershipView is an account's seat within one game.
//
// PlayerID is the seat's row id, which is also the engine's player_id — the
// same identity agentSeatView carries, because the engine must not care whether
// a seat is held by a person or by code. It is what path-scoped endpoints
// address a seat by, so a client that can see a roster can act on it without
// having to send an account id back.
type membershipView struct {
	PlayerID    int64     `json:"player_id"`
	GameID      int64     `json:"game_id"`
	GameName    string    `json:"game_name"`
	AccountID   int64     `json:"account_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	IsGM        bool      `json:"is_gm"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// newMembershipView renders a membership.
func newMembershipView(m *store.Membership) membershipView {
	return membershipView{
		PlayerID:    m.ID,
		GameID:      m.GameID,
		GameName:    m.GameName,
		AccountID:   m.AccountID,
		Email:       m.Email,
		DisplayName: m.DisplayName,
		IsGM:        m.IsGM,
		IsActive:    m.IsActive,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

// seedView is the pair of words a game's random stream is built from.
//
// Both are decimal **strings** rather than JSON numbers, and that is the one
// place this API departs from sending an integer as an integer. A seed word is
// a full-range uint64; a JSON number is an IEEE 754 double in every browser, so
// any value above 2^53 would reach the client rounded and come back changed. A
// seed that does not round-trip exactly makes a game unreplayable, which is the
// single property internal/engine exists to guarantee, so the wire format gives
// that up before it gives up exactness.
type seedView struct {
	Hi string `json:"hi"`
	Lo string `json:"lo"`
}

// newSeedView renders a seed for the wire.
func newSeedView(s engine.Seed) seedView {
	return seedView{
		Hi: strconv.FormatUint(s.Hi, 10),
		Lo: strconv.FormatUint(s.Lo, 10),
	}
}

// gameStateView is how far a game has got.
type gameStateView struct {
	GameID    int64     `json:"game_id"`
	Turn      int       `json:"turn"`
	Seed      seedView  `json:"seed"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// newGameStateView renders a game's state.
func newGameStateView(s *store.GameState) gameStateView {
	return gameStateView{
		GameID:    s.GameID,
		Turn:      s.Turn,
		Seed:      seedView{Hi: strconv.FormatUint(s.SeedHi, 10), Lo: strconv.FormatUint(s.SeedLo, 10)},
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

// playerGameView is one game as the account seated at it sees it.
//
// State is null until a game master has set the game up. The client shows a
// different page in each case — a player is told to come back later, a game
// master is given the form — so the distinction has to survive onto the wire
// rather than being flattened into an empty state object.
//
// DefaultSeed is present only when this caller can act on it: the game master
// of a game with no state. It exists to fill in that one form, so sending it to
// a player who cannot submit the form would only invite the question of what
// they are supposed to do with it.
type playerGameView struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	IsActive    bool           `json:"is_active"`
	IsGM        bool           `json:"is_gm"`
	State       *gameStateView `json:"state"`
	DefaultSeed *seedView      `json:"default_seed,omitempty"`
}

// agentView is one agent this build can play, as offered to a game master
// choosing one.
//
// It is rendered from an engine.Descriptor rather than from a database row,
// because which agents exist is a property of this binary. A catalogue read
// from the database could advertise an agent the running code cannot play.
type agentView struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// newAgentView renders one entry of the engine's catalogue.
func newAgentView(d engine.Descriptor) agentView {
	return agentView{
		Key:         d.Key,
		Name:        d.Name,
		Description: d.Description,
	}
}

// agentSeatView is an agent seated in a game.
//
// player_id is on the wire because it is the identity the engine uses, and a
// game master looking at engine output needs to be able to match a faction's
// controller to a seat. It is the seat's row id; there is no separate account.
type agentSeatView struct {
	PlayerID  int64  `json:"player_id"`
	GameID    int64  `json:"game_id"`
	GameName  string `json:"game_name"`
	AgentKey  string `json:"agent_key"`
	AgentName string `json:"agent_name"`
	IsActive  bool   `json:"is_active"`
	// Playable reports whether this build still has the implementation the
	// seat names. It is computed, not stored: a database written by another
	// release can hold a key this binary does not know, and a game master
	// should see that in the listing rather than discover it at turn
	// resolution.
	Playable  bool      `json:"playable"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// newAgentSeatView renders a seated agent, resolving its key against this
// build's catalogue.
func newAgentSeatView(s *store.AgentSeat) agentSeatView {
	_, playable := engine.AgentByKey(s.AgentKey)
	return agentSeatView{
		PlayerID:  s.ID,
		GameID:    s.GameID,
		GameName:  s.GameName,
		AgentKey:  s.AgentKey,
		AgentName: s.AgentName,
		IsActive:  s.IsActive,
		Playable:  playable,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

// healthView is the body of the liveness and readiness endpoints.
type healthView struct {
	Status   string `json:"status"`
	Database string `json:"database,omitempty"`
	// Migration is the database's applied migration number; Latest is the
	// newest this binary knows. They differ when a read-only process is running
	// against a database migrated by a newer build.
	Migration int    `json:"migration,omitempty"`
	Latest    int    `json:"latest,omitempty"`
	ReadOnly  bool   `json:"read_only"`
	Version   string `json:"version"`
}
