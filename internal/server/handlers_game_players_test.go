// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// The roster endpoints are one rule wearing three hats — a game master may
// change player seats, and a GM seat is an administrator's business — so these
// tests are mostly about what is *refused*. Each case is a real account with a
// real session cookie, because the rule is about which seat the caller and the
// target hold and nothing else would exercise it.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// playerResponse mirrors the roster fields a client acts on.
type playerResponse struct {
	PlayerID    int64  `json:"player_id"`
	AccountID   int64  `json:"account_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	IsGM        bool   `json:"is_gm"`
	IsActive    bool   `json:"is_active"`
}

// roster reads a game's players as the holder of cookie sees them.
func roster(t *testing.T, srv *Server, cookie *http.Cookie, gameID int64) []playerResponse {
	t.Helper()
	recorder := do(t, srv, cookie, http.MethodGet, "/api/v1/games/"+itoa(gameID)+"/players", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list players: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	var players []playerResponse
	decodeData(t, recorder, &players)
	return players
}

// find returns the roster entry for an address, failing if it is absent.
func find(t *testing.T, players []playerResponse, email string) playerResponse {
	t.Helper()
	for _, player := range players {
		if player.Email == email {
			return player
		}
	}
	t.Fatalf("%s is not on the roster: %+v", email, players)
	return playerResponse{}
}

func TestListPlayersShowsEveryHumanSeat(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Roster")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")

	players := roster(t, srv, gm, gameID)
	if len(players) != 2 {
		t.Fatalf("roster has %d entries, want 2: %+v", len(players), players)
	}
	master := find(t, players, "gm1@example.com")
	if !master.IsGM {
		t.Error("the game master's own seat is not marked is_gm")
	}
	player := find(t, players, "user1@example.com")
	if player.IsGM {
		t.Error("a player's seat is marked is_gm")
	}
	// player_id is what every other endpoint here addresses a seat by.
	if player.PlayerID == 0 {
		t.Error("player_id is absent; the roster cannot be acted on without it")
	}
	if player.DisplayName == "" {
		t.Error("display_name is absent; the roster is a list of people")
	}
}

// A removed player is a deactivated seat, so hiding those would leave a game
// master unable to see or undo a removal.
func TestListPlayersIncludesDeactivatedSeats(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Departed roster")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")

	seat := find(t, roster(t, srv, gm, gameID), "user1@example.com")
	recorder := do(t, srv, gm, http.MethodPatch,
		"/api/v1/games/"+itoa(gameID)+"/players/"+itoa(seat.PlayerID), `{"is_active":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("deactivate: status %d, body %s", recorder.Code, recorder.Body.String())
	}

	after := find(t, roster(t, srv, gm, gameID), "user1@example.com")
	if after.IsActive {
		t.Error("is_active = true after deactivating")
	}
	if after.PlayerID != seat.PlayerID {
		t.Errorf("player_id changed from %d to %d", seat.PlayerID, after.PlayerID)
	}
}

func TestAddPlayerByEmail(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Invites")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/players",
		`{"email":"user1@example.com"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}
	var added playerResponse
	decodeData(t, recorder, &added)
	if added.IsGM {
		t.Error("is_gm = true when the request omitted it; a plain player is the default")
	}
	if !added.IsActive {
		t.Error("is_active = false for a freshly added player")
	}
	if added.Email != "user1@example.com" {
		t.Errorf("email = %q, want %q", added.Email, "user1@example.com")
	}
}

func TestAddPlayerAsGameMaster(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Co-masters")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/players",
		`{"email":"user1@example.com","is_gm":true}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}
	var added playerResponse
	decodeData(t, recorder, &added)
	if !added.IsGM {
		t.Error("is_gm = false when the request asked for a game master")
	}
}

// Every reason an address cannot be seated answers alike, so that running a
// game does not become a way to find out which accounts exist.
func TestAddPlayerRefusesWhatCannotBeSeated(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Refusals")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	cases := []struct{ name, email string }{
		{"no such account", "nobody@example.com"},
		{"an administrator", "admin@example.com"},
		{"not an address at all", "not-an-address"},
	}
	var messages []string
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, srv, gm, http.MethodPost,
				"/api/v1/games/"+itoa(gameID)+"/players",
				`{"email":"`+testCase.email+`"}`)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body %s", recorder.Code, recorder.Body.String())
			}
			var problem struct {
				Detail string `json:"detail"`
				Errors []struct {
					Field   string `json:"field"`
					Message string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if len(problem.Errors) == 0 || problem.Errors[0].Field != "email" {
				t.Fatalf("errors = %+v, want one naming email", problem.Errors)
			}
			messages = append(messages, problem.Detail+"|"+problem.Errors[0].Message)
		})
	}
	// The point of the shared message: an administrator's address and an
	// address belonging to nobody must be indistinguishable from out here.
	for i := 1; i < len(messages); i++ {
		if messages[i] != messages[0] {
			t.Errorf("refusals differ and so leak which addresses exist:\n  %q\n  %q",
				messages[0], messages[i])
		}
	}
}

// Adding is create, not save. Being wrong about whether somebody is already in
// the game should be reported, not resolved by overwriting a seat.
func TestAddPlayerRefusesAnExistingSeat(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Twice")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/players",
		`{"email":"user1@example.com","is_gm":true}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body.String())
	}
	// And the seat it refused to overwrite is untouched.
	existing := find(t, roster(t, srv, gm, gameID), "user1@example.com")
	if existing.IsGM {
		t.Error("the existing seat was promoted by a refused add")
	}
}

func TestPromotePlayerToGameMaster(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Promotion")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")

	seat := find(t, roster(t, srv, gm, gameID), "user1@example.com")
	recorder := do(t, srv, gm, http.MethodPatch,
		"/api/v1/games/"+itoa(gameID)+"/players/"+itoa(seat.PlayerID), `{"is_gm":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	var promoted playerResponse
	decodeData(t, recorder, &promoted)
	if !promoted.IsGM {
		t.Error("is_gm = false after promotion")
	}
	if !promoted.IsActive {
		t.Error("promotion deactivated the seat")
	}
	// The promoted account can now run the game.
	promotedCookie := signIn(t, db, "user1@example.com")
	if got := roster(t, srv, promotedCookie, gameID); len(got) != 2 {
		t.Errorf("the promoted game master reads a roster of %d, want 2", len(got))
	}
}

// Promotion is one-way. The two ways to try to undo it are the two things a
// game master is not allowed to do to a GM seat.
func TestGameMasterSeatsAreAdministratorTerritory(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "One way")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	players := roster(t, srv, gm, gameID)
	other := find(t, players, "user1@example.com")
	own := find(t, players, "gm1@example.com")

	cases := []struct{ name, path, body string }{
		{"demote another game master", itoa(other.PlayerID), `{"is_gm":false}`},
		{"deactivate another game master", itoa(other.PlayerID), `{"is_active":false}`},
		{"demote yourself", itoa(own.PlayerID), `{"is_gm":false}`},
		{"deactivate yourself", itoa(own.PlayerID), `{"is_active":false}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, srv, gm, http.MethodPatch,
				"/api/v1/games/"+itoa(gameID)+"/players/"+testCase.path, testCase.body)
			if recorder.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403; body %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// Nothing moved.
	after := roster(t, srv, gm, gameID)
	for _, player := range after {
		if !player.IsGM || !player.IsActive {
			t.Errorf("%s ended up is_gm=%v is_active=%v; both seats should be untouched",
				player.Email, player.IsGM, player.IsActive)
		}
	}
}

// An administrator is not bound by any of it, which is what makes the rule
// above safe to be strict: every refusal has somewhere to go.
func TestAnAdministratorCanStillDemoteAGameMaster(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Escape hatch")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", true)

	account, err := db.AccountByEmail(context.Background(), "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	recorder := do(t, srv, admin, http.MethodPut,
		"/api/v1/admin/games/"+itoa(gameID)+"/memberships/"+itoa(account.ID),
		`{"is_gm":false,"is_active":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}

	gm := signIn(t, db, "gm1@example.com")
	demoted := find(t, roster(t, srv, gm, gameID), "user1@example.com")
	if demoted.IsGM || demoted.IsActive {
		t.Errorf("admin demotion did not stick: is_gm=%v is_active=%v", demoted.IsGM, demoted.IsActive)
	}
}

// The administrator's escape hatch has one limit: it must not be used to leave
// a game with nobody able to run it. A game master cannot change a GM seat, and
// an administrator holds no seat, so a game stranded this way could not be
// repaired from inside it.
func TestAdministratorCannotStrandAGameWithoutAGameMaster(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Last master")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)

	account, err := db.AccountByEmail(context.Background(), "gm1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	path := "/api/v1/admin/games/" + itoa(gameID) + "/memberships/" + itoa(account.ID)

	// Both ways of taking the last game master away are the same mistake.
	for _, testCase := range []struct{ name, body string }{
		{"demoting", `{"is_gm":false,"is_active":true}`},
		{"deactivating", `{"is_gm":true,"is_active":false}`},
		{"both at once", `{"is_gm":false,"is_active":false}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, srv, admin, http.MethodPut, path, testCase.body)
			if recorder.Code != http.StatusConflict {
				t.Errorf("status = %d, want 409; body %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// The seat is untouched, and the game is still runnable.
	gm := signIn(t, db, "gm1@example.com")
	master := find(t, roster(t, srv, gm, gameID), "gm1@example.com")
	if !master.IsGM || !master.IsActive {
		t.Errorf("the last game master ended up is_gm=%v is_active=%v", master.IsGM, master.IsActive)
	}

	// Promoting a replacement is the whole of the fix, and the message says so.
	player := find(t, roster(t, srv, gm, gameID), "user1@example.com")
	recorder := do(t, srv, gm, http.MethodPatch,
		"/api/v1/games/"+itoa(gameID)+"/players/"+itoa(player.PlayerID), `{"is_gm":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("promote a replacement: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	recorder = do(t, srv, admin, http.MethodPut, path, `{"is_gm":false,"is_active":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("demote once a replacement exists: status %d, want 200; body %s",
			recorder.Code, recorder.Body.String())
	}

	// And the replacement is now the last one, so the guard has moved with it.
	replacement, err := db.AccountByEmail(context.Background(), "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	recorder = do(t, srv, admin, http.MethodPut,
		"/api/v1/admin/games/"+itoa(gameID)+"/memberships/"+itoa(replacement.ID),
		`{"is_gm":false,"is_active":true}`)
	if recorder.Code != http.StatusConflict {
		t.Errorf("demoting the new last master: status = %d, want 409; body %s",
			recorder.Code, recorder.Body.String())
	}
}

// A game with no game master at all is not stranded — there is nothing to
// preserve — so adding and editing plain players stays unguarded.
func TestTheGuardOnlyProtectsAnExistingGameMaster(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "No master")

	account, err := db.AccountByEmail(context.Background(), "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	path := "/api/v1/admin/games/" + itoa(gameID) + "/memberships/" + itoa(account.ID)

	// Creating a plain seat in a game that has no master.
	if recorder := do(t, srv, admin, http.MethodPut, path,
		`{"is_gm":false,"is_active":true}`); recorder.Code != http.StatusOK {
		t.Fatalf("create a player: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	// And deactivating that plain seat.
	if recorder := do(t, srv, admin, http.MethodPut, path,
		`{"is_gm":false,"is_active":false}`); recorder.Code != http.StatusOK {
		t.Errorf("deactivate a player: status %d, want 200; body %s",
			recorder.Code, recorder.Body.String())
	}
}

// The whole roster is the game master's, so a player at the same table reaches
// none of it — and neither does an administrator, who holds no seat.
func TestRosterIsTheGameMastersAlone(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Guarded roster")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")
	seat := find(t, roster(t, srv, gm, gameID), "user1@example.com")

	base := "/api/v1/games/" + itoa(gameID) + "/players"
	requests := []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPost, base, `{"email":"user2@example.com"}`},
		{http.MethodPatch, base + "/" + itoa(seat.PlayerID), `{"is_active":false}`},
	}
	callers := []struct {
		name   string
		cookie *http.Cookie
		status int
	}{
		{"a player at the table", signIn(t, db, "user1@example.com"), http.StatusForbidden},
		{"someone with no seat", signIn(t, db, "user2@example.com"), http.StatusNotFound},
		{"an administrator", admin, http.StatusNotFound},
		{"anonymous", nil, http.StatusUnauthorized},
	}
	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			for _, request := range requests {
				recorder := do(t, srv, caller.cookie, request.method, request.path, request.body)
				if recorder.Code != caller.status {
					t.Errorf("%s %s: status = %d, want %d; body %s",
						request.method, request.path, recorder.Code, caller.status,
						recorder.Body.String())
				}
			}
		})
	}
}

// A seat id from one game must not reach another game's seat.
func TestPlayerSeatIsScopedToItsGame(t *testing.T) {
	srv, admin, db := testServer(t)
	mine := createGame(t, srv, admin, "Mine roster")
	yours := createGame(t, srv, admin, "Yours roster")
	seatAccount(t, srv, admin, db, mine, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, yours, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, yours, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")

	seat := find(t, roster(t, srv, gm, yours), "user1@example.com")
	recorder := do(t, srv, gm, http.MethodPatch,
		"/api/v1/games/"+itoa(mine)+"/players/"+itoa(seat.PlayerID), `{"is_active":false}`)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body %s", recorder.Code, recorder.Body.String())
	}
}

func TestUpdatePlayerRejectsEmptyBody(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Empty patch")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")

	seat := find(t, roster(t, srv, gm, gameID), "user1@example.com")
	recorder := do(t, srv, gm, http.MethodPatch,
		"/api/v1/games/"+itoa(gameID)+"/players/"+itoa(seat.PlayerID), `{}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422; body %s", recorder.Code, recorder.Body.String())
	}
}
