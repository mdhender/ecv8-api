// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// These tests drive the real router the way a browser does, with a real session
// cookie for a real seeded account, because the contract worth protecting here
// is as much about *who* gets an answer as about what the answer is. Every case
// that matters — a player, that game's master, someone with no seat, an
// administrator — is a different account rather than a different flag.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/engine"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/tokens"
)

// signIn returns a session cookie for a seeded account, minted the way a login
// does rather than by reaching past the middleware.
func signIn(t *testing.T, db *store.DB, email string) *http.Cookie {
	t.Helper()
	ctx := context.Background()

	account, err := db.AccountByEmail(ctx, email)
	if err != nil {
		t.Fatalf("load seeded account %s: %v", email, err)
	}
	token, err := tokens.New()
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	cfg := config.Default()
	now := store.Now()
	if _, err := db.CreateSession(ctx, store.NewSession{
		AccountID: account.ID,
		TokenHash: tokens.Fingerprint(token),
		ExpiresAt: now.Add(cfg.SessionTTL),
		UserAgent: "test",
		RemoteIP:  "127.0.0.1",
	}, now); err != nil {
		t.Fatalf("create session for %s: %v", email, err)
	}
	return &http.Cookie{Name: cfg.CookieName, Value: token}
}

// seatAccount puts a seeded account in a game through the admin API, so the
// seat under test is one an administrator could really have created.
func seatAccount(t *testing.T, srv *Server, admin *http.Cookie, db *store.DB, gameID int64, email string, isGM bool) {
	t.Helper()
	account, err := db.AccountByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("load seeded account %s: %v", email, err)
	}
	body := `{"is_gm":` + strconv.FormatBool(isGM) + `,"is_active":true}`
	recorder := do(t, srv, admin, http.MethodPut,
		"/api/v1/admin/games/"+itoa(gameID)+"/memberships/"+itoa(account.ID), body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("seat %s: status %d, body %s", email, recorder.Code, recorder.Body.String())
	}
}

// playerGameResponse mirrors the fields the client branches on.
type playerGameResponse struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
	IsGM     bool   `json:"is_gm"`
	State    *struct {
		GameID int64 `json:"game_id"`
		Turn   int   `json:"turn"`
		Seed   struct {
			Hi string `json:"hi"`
			Lo string `json:"lo"`
		} `json:"seed"`
	} `json:"state"`
	DefaultSeed *struct {
		Hi string `json:"hi"`
		Lo string `json:"lo"`
	} `json:"default_seed"`
}

// getGame reads one game as the holder of cookie sees it.
func getGame(t *testing.T, srv *Server, cookie *http.Cookie, gameID int64) playerGameResponse {
	t.Helper()
	recorder := do(t, srv, cookie, http.MethodGet, "/api/v1/games/"+itoa(gameID), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	var game playerGameResponse
	decodeData(t, recorder, &game)
	return game
}

// A game with no state is what a player sees before the game master has been
// in, and the client shows them a "come back later" page on the strength of it.
// A null state and a false is_gm are therefore both load-bearing.
func TestPlayerSeesAGameThatIsNotSetUpYet(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Unstarted")
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)

	game := getGame(t, srv, signIn(t, db, "user1@example.com"), gameID)
	if game.State != nil {
		t.Errorf("state = %+v, want null for a game that has not been set up", game.State)
	}
	if game.IsGM {
		t.Error("is_gm = true for a player seat")
	}
	if game.Name != "Unstarted" {
		t.Errorf("name = %q, want %q", game.Name, "Unstarted")
	}
	// A player cannot submit the setup form, so offering them its starting
	// values would only raise a question they have no way to act on.
	if game.DefaultSeed != nil {
		t.Errorf("default_seed = %+v, want absent for a player", game.DefaultSeed)
	}
}

// The game master gets the form, and the values it starts from come from the
// API rather than from the client — that is what keeps one default in one place.
func TestGameMasterIsOfferedTheDefaultSeed(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Awaiting setup")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)

	game := getGame(t, srv, signIn(t, db, "gm1@example.com"), gameID)
	if !game.IsGM {
		t.Fatal("is_gm = false for a game master's seat")
	}
	if game.State != nil {
		t.Fatalf("state = %+v, want null", game.State)
	}
	if game.DefaultSeed == nil {
		t.Fatal("default_seed is absent; the game master has nothing to prefill the form with")
	}
	want := engine.DefaultSeed()
	if game.DefaultSeed.Hi != strconv.FormatUint(want.Hi, 10) ||
		game.DefaultSeed.Lo != strconv.FormatUint(want.Lo, 10) {
		t.Errorf("default_seed = %s/%s, want the engine's %d/%d",
			game.DefaultSeed.Hi, game.DefaultSeed.Lo, want.Hi, want.Lo)
	}
}

// Omitting the seed is the ordinary case: a game master with no reason to
// choose a stream should be able to start a game without inventing two numbers.
func TestCreateGameStateDefaultsTheSeed(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Defaulted")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/state", `{}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}

	want := engine.DefaultSeed()
	var created struct {
		Turn int `json:"turn"`
		Seed struct {
			Hi string `json:"hi"`
			Lo string `json:"lo"`
		} `json:"seed"`
	}
	decodeData(t, recorder, &created)
	if created.Turn != 0 {
		t.Errorf("turn = %d, want 0; a new game starts at setup", created.Turn)
	}
	if created.Seed.Hi != strconv.FormatUint(want.Hi, 10) ||
		created.Seed.Lo != strconv.FormatUint(want.Lo, 10) {
		t.Errorf("seed = %s/%s, want the engine's %d/%d",
			created.Seed.Hi, created.Seed.Lo, want.Hi, want.Lo)
	}

	// Once a game is set up there is no form left to fill in, so the starting
	// values stop being offered.
	game := getGame(t, srv, gm, gameID)
	if game.State == nil {
		t.Fatal("state is null immediately after it was created")
	}
	if game.State.Turn != 0 {
		t.Errorf("turn = %d, want 0", game.State.Turn)
	}
	if game.DefaultSeed != nil {
		t.Errorf("default_seed = %+v, want absent once the game is set up", game.DefaultSeed)
	}
}

// The upper half of the uint64 range is exactly what a JSON number would lose,
// and a seed that comes back changed makes the game unreplayable. This is the
// case the string wire format exists for.
func TestCreateGameStateRoundTripsALargeSeed(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Large seed")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	const hi = "18446744073709551615" // 2^64 - 1
	const lo = "9007199254740993"     // 2^53 + 1, the first integer a double cannot hold

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/state",
		`{"seed":{"hi":"`+hi+`","lo":"`+lo+`"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}

	game := getGame(t, srv, gm, gameID)
	if game.State == nil {
		t.Fatal("state is null after it was created")
	}
	if game.State.Seed.Hi != hi || game.State.Seed.Lo != lo {
		t.Errorf("seed = %s/%s, want %s/%s exactly",
			game.State.Seed.Hi, game.State.Seed.Lo, hi, lo)
	}
}

func TestCreateGameStateRejectsAnInvalidSeed(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Bad seed")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	cases := []struct{ name, body, field string }{
		{"negative", `{"seed":{"hi":"-1","lo":"42"}}`, "seed.hi"},
		{"fractional", `{"seed":{"hi":"19","lo":"42.5"}}`, "seed.lo"},
		{"overflowing", `{"seed":{"hi":"18446744073709551616","lo":"42"}}`, "seed.hi"},
		{"empty", `{"seed":{"hi":"","lo":""}}`, "seed.hi"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, srv, gm, http.MethodPost,
				"/api/v1/games/"+itoa(gameID)+"/state", testCase.body)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body %s", recorder.Code, recorder.Body.String())
			}
			var problem struct {
				Errors []struct {
					Field string `json:"field"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if len(problem.Errors) == 0 || problem.Errors[0].Field != testCase.field {
				t.Errorf("errors = %+v, want the first to name %q", problem.Errors, testCase.field)
			}
		})
	}
}

// Setting a game up is the game master's act. A player at the same table is
// authorised by the same seat lookup and still refused.
func TestCreateGameStateIsTheGameMastersAlone(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Guarded setup")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)

	recorder := do(t, srv, signIn(t, db, "user1@example.com"), http.MethodPost,
		"/api/v1/games/"+itoa(gameID)+"/state", `{}`)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("player status = %d, want 403; body %s", recorder.Code, recorder.Body.String())
	}
}

// The seed is what makes every later turn reproducible, so a second setup would
// invalidate the turns already resolved. It is refused rather than applied.
func TestGameStateIsWrittenOnlyOnce(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Once")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	first := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/state", `{}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first setup: status %d, body %s", first.Code, first.Body.String())
	}
	second := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/state",
		`{"seed":{"hi":"1","lo":"2"}}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second setup: status = %d, want 409; body %s", second.Code, second.Body.String())
	}

	want := engine.DefaultSeed()
	game := getGame(t, srv, gm, gameID)
	if game.State == nil || game.State.Seed.Hi != strconv.FormatUint(want.Hi, 10) {
		t.Errorf("state = %+v, want the seed the first call wrote", game.State)
	}
}

// Authorisation is the seat. An account with no seat learns nothing about the
// game — including whether it exists — and an administrator holds no seat.
func TestGamesAreVisibleOnlyToTheirSeats(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Private")
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)

	cases := []struct {
		name   string
		cookie *http.Cookie
		status int
	}{
		{"unseated user", signIn(t, db, "user2@example.com"), http.StatusNotFound},
		{"administrator", admin, http.StatusNotFound},
		{"anonymous", nil, http.StatusUnauthorized},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, srv, testCase.cookie, http.MethodGet, "/api/v1/games/"+itoa(gameID), "")
			if recorder.Code != testCase.status {
				t.Errorf("GET status = %d, want %d; body %s",
					recorder.Code, testCase.status, recorder.Body.String())
			}
			recorder = do(t, srv, testCase.cookie, http.MethodPost,
				"/api/v1/games/"+itoa(gameID)+"/state", `{}`)
			if recorder.Code != testCase.status {
				t.Errorf("POST status = %d, want %d; body %s",
					recorder.Code, testCase.status, recorder.Body.String())
			}
		})
	}
}

// Removing a player from a game deactivates their seat. A removed player should
// not still be able to read the game.
func TestAnInactiveSeatCannotReadItsGame(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Departed")
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	player := signIn(t, db, "user1@example.com")

	account, err := db.AccountByEmail(context.Background(), "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	recorder := do(t, srv, admin, http.MethodPut,
		"/api/v1/admin/games/"+itoa(gameID)+"/memberships/"+itoa(account.ID),
		`{"is_gm":false,"is_active":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("deactivate seat: status %d, body %s", recorder.Code, recorder.Body.String())
	}

	recorder = do(t, srv, player, http.MethodGet, "/api/v1/games/"+itoa(gameID), "")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body %s", recorder.Code, recorder.Body.String())
	}
}

// Setting up a game nobody is playing is a mistake worth reporting, which is
// the same rule that keeps agents out of an inactive game.
func TestCreateGameStateRefusesAnInactiveGame(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Closed game")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	recorder := do(t, srv, admin, http.MethodPatch,
		"/api/v1/admin/games/"+itoa(gameID), `{"is_active":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("deactivate game: status %d, body %s", recorder.Code, recorder.Body.String())
	}

	recorder = do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/state", `{}`)
	if recorder.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body %s", recorder.Code, recorder.Body.String())
	}

	// The game master can still see the game, and can see that it is closed.
	game := getGame(t, srv, gm, gameID)
	if game.IsActive {
		t.Error("is_active = true for a deactivated game")
	}
}
