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
//
// Seed is absent unless the caller is the game's master. The stream is
// reproducible by design — that is the whole point of internal/engine — so a
// player who knows the seed can run the generator forward and read the outcome
// of events before they are resolved. Turn resolution being auditable is for
// the person running the game, not for the people playing against it.
type gameStateView struct {
	GameID    int64     `json:"game_id"`
	Turn      int       `json:"turn"`
	Seed      *seedView `json:"seed,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// newGameStateView renders a game's state.
//
// withSeed has no default and is not derived from the state, because the
// question it answers is about the caller: only a game master may see the seed.
// Every call site has to say which it is holding, so a new one cannot leak the
// seed by forgetting the rule existed.
func newGameStateView(s *store.GameState, withSeed bool) gameStateView {
	view := gameStateView{
		GameID:    s.GameID,
		Turn:      s.Turn,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
	if withSeed {
		view.Seed = &seedView{Hi: strconv.FormatUint(s.SeedHi, 10), Lo: strconv.FormatUint(s.SeedLo, 10)}
	}
	return view
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
// they are supposed to do with it. Once the game has been set up, State carries
// the seed under the same rule and for the stronger reason gameStateView gives:
// a player who has it can predict the game.
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

// clusterGeneratorView is one cluster generator this build can run, as offered
// to a game master choosing one.
//
// It is rendered from the engine's catalogue rather than from a table, for the
// reason agentView gives: which generators exist is a property of this binary,
// and a list read from the database could offer one the running code cannot
// run.
type clusterGeneratorView struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// newClusterGeneratorView renders one entry of the engine's generator catalogue.
func newClusterGeneratorView(d engine.GeneratorDescriptor) clusterGeneratorView {
	return clusterGeneratorView{
		Key:         d.Key,
		Name:        d.Name,
		Description: d.Description,
	}
}

// clusterOptionsView is everything the generate form needs in order to offer a
// starting point and stay inside the rules.
//
// The bounds travel because the client must not hold its own copy of them. A
// range duplicated in the browser is wrong the first time the engine changes
// its mind, and it would be wrong silently — the form would refuse a value the
// server accepts, or offer one it rejects. This is the same rule that sends
// default_seed instead of letting the setup form invent one.
type clusterOptionsView struct {
	Generators       []clusterGeneratorView `json:"generators"`
	Generator        string                 `json:"generator"`
	StelliumCount    int                    `json:"stellium_count"`
	Radius           int                    `json:"radius"`
	MinStelliumCount int                    `json:"min_stellium_count"`
	MaxStelliumCount int                    `json:"max_stellium_count"`
	MinRadius        int                    `json:"min_radius"`
	MaxRadius        int                    `json:"max_radius"`
}

// stelliumView is one stellium on the map.
//
// The id and the coordinates both travel because they are different things: the
// reference identifies a stellium by its integer id and displays it as
// (x, y, z), and the two are needed together the moment a report has to be read
// alongside a map.
type stelliumView struct {
	ID int64 `json:"id"`
	X  int   `json:"x"`
	Y  int   `json:"y"`
	Z  int   `json:"z"`
}

// clusterView is a game's map, with the parameters it was generated from.
//
// The parameters are on the wire rather than kept for the server, because they
// are what makes the map checkable: together with the game's seed they say what
// would have to be repeated to get the same map, and a game master who cannot
// see them has no way to tell one generated cluster from another.
type clusterView struct {
	GameID        int64          `json:"game_id"`
	Generator     string         `json:"generator"`
	StelliumCount int            `json:"stellium_count"`
	Radius        int            `json:"radius"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	Stelliums     []stelliumView `json:"stelliums"`
}

// newClusterView renders a cluster and its stelliums.
func newClusterView(c *store.Cluster, stelliums []store.Stellium) clusterView {
	view := clusterView{
		GameID:        c.GameID,
		Generator:     c.GeneratorKey,
		StelliumCount: c.StelliumCount,
		Radius:        c.Radius,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
		Stelliums:     make([]stelliumView, 0, len(stelliums)),
	}
	for _, s := range stelliums {
		view.Stelliums = append(view.Stelliums, stelliumView{ID: s.ID, X: s.X, Y: s.Y, Z: s.Z})
	}
	return view
}

// gameClusterView is the cluster page: the map if there is one, what it would
// take to make one if there is not, and who is asking.
//
// Three states have to survive onto the wire, because the page shows something
// different for each and the server is what decides which: a game that has not
// been set up cannot have a map at all, since a cluster is drawn from the seed
// that setting up writes; a game that has been set up but has no cluster gets
// the form; and a game with a cluster gets the map. Flattening the first two
// into "no cluster" would leave the client to guess why, and it would guess by
// re-deriving a rule the engine owns.
//
// The map itself is the same for everybody at the table. Options is present
// only when the form can be submitted, so a player never receives it — matching
// the way default_seed accompanies only a game the caller can set up. IsGM is
// what remains for a page to word an empty map with: "you have not generated
// this yet" and "it has not been generated yet" are the same fact told to
// different readers, and deriving which from the absence of Options would make
// the wording depend on a rule about forms.
type gameClusterView struct {
	GameID   int64  `json:"game_id"`
	GameName string `json:"game_name"`
	IsActive bool   `json:"is_active"`
	IsGM     bool   `json:"is_gm"`
	// IsSetUp reports whether the game has the state a cluster is drawn from.
	IsSetUp bool                `json:"is_set_up"`
	Cluster *clusterView        `json:"cluster"`
	Options *clusterOptionsView `json:"options,omitempty"`
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
