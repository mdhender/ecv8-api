// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// These tests drive the real router with real requests, the way a client does,
// because what is worth protecting is the endpoint's contract: the status it
// returns, the fields it emits, and what it refuses. They authenticate through
// a genuine session cookie rather than bypassing the middleware, so a change
// that broke authorisation would fail here too.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/engine"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/tokens"
)

// testServer builds a server over its own in-memory database, and returns it
// with a cookie for an authenticated administrator.
func testServer(t *testing.T) (*Server, *http.Cookie, *store.DB) {
	t.Helper()
	ctx := context.Background()

	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, t.Name())

	db, err := store.OpenTemporaryStore(ctx, name)
	if err != nil {
		t.Fatalf("open temporary store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	cfg := config.Default()
	// The test client speaks plain HTTP to an httptest recorder, so a Secure
	// cookie would never come back.
	cfg.CookieSecure = false

	srv, err := New(&cfg, db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	admin, err := db.AccountByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatalf("load seeded administrator: %v", err)
	}
	token, err := tokens.New()
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	now := store.Now()
	if _, err := db.CreateSession(ctx, store.NewSession{
		AccountID: admin.ID,
		TokenHash: tokens.Fingerprint(token),
		ExpiresAt: now.Add(cfg.SessionTTL),
		UserAgent: "test",
		RemoteIP:  "127.0.0.1",
	}, now); err != nil {
		t.Fatalf("create session: %v", err)
	}

	return srv, &http.Cookie{Name: cfg.CookieName, Value: token}, db
}

// do sends one request through the router and returns the recorded response.
func do(t *testing.T, srv *Server, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	srv.echo.ServeHTTP(recorder, request)
	return recorder
}

// decodeData unwraps the data envelope every successful response carries.
func decodeData(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, recorder.Body.String())
	}
	if err := json.Unmarshal(envelope.Data, into); err != nil {
		t.Fatalf("decode data: %v\nbody: %s", err, recorder.Body.String())
	}
}

// createGame makes a game to seat agents in, through the API.
func createGame(t *testing.T, srv *Server, cookie *http.Cookie, name string) int64 {
	t.Helper()
	recorder := do(t, srv, cookie, http.MethodPost, "/api/v1/admin/games",
		`{"name":"`+name+`"}`)
	if recorder.Code != http.StatusCreated && recorder.Code != http.StatusOK {
		t.Fatalf("create game: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	var game struct {
		ID int64 `json:"id"`
	}
	decodeData(t, recorder, &game)
	return game.ID
}

// The catalogue is what a game master chooses from, so it must agree with the
// engine exactly — not with anything stored.
func TestListAgentsServesTheEngineCatalogue(t *testing.T) {
	srv, cookie, _ := testServer(t)

	recorder := do(t, srv, cookie, http.MethodGet, "/api/v1/admin/agents", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}

	var got []struct {
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	decodeData(t, recorder, &got)

	want := engine.Agents()
	if len(got) != len(want) {
		t.Fatalf("served %d agents, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Key != want[i].Key || got[i].Name != want[i].Name {
			t.Errorf("agent %d = %q/%q, want %q/%q",
				i, got[i].Key, got[i].Name, want[i].Key, want[i].Name)
		}
		if got[i].Description == "" {
			t.Errorf("agent %q served without a description", got[i].Key)
		}
	}
}

func TestListAgentsRequiresAdmin(t *testing.T) {
	srv, _, _ := testServer(t)

	recorder := do(t, srv, nil, http.MethodGet, "/api/v1/admin/agents", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want 401; body %s", recorder.Code, recorder.Body.String())
	}
}

type seatResponse struct {
	PlayerID  int64  `json:"player_id"`
	GameID    int64  `json:"game_id"`
	AgentKey  string `json:"agent_key"`
	AgentName string `json:"agent_name"`
	IsActive  bool   `json:"is_active"`
	Playable  bool   `json:"playable"`
}

// Omitting agent_name is the common case, and it must produce a usable label
// rather than an empty one.
func TestCreateAgentSeatDefaultsTheName(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "Defaults")
	first := engine.Agents()[0]

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"`+first.Key+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}

	var seat seatResponse
	decodeData(t, recorder, &seat)
	if seat.AgentName != first.Name {
		t.Errorf("agent_name = %q, want the agent's own name %q", seat.AgentName, first.Name)
	}
	if seat.AgentKey != first.Key {
		t.Errorf("agent_key = %q, want %q", seat.AgentKey, first.Key)
	}
	if seat.PlayerID == 0 {
		t.Error("player_id is absent; it is what the engine refers to")
	}
	if !seat.Playable {
		t.Error("playable = false for an agent this build has")
	}
	if !seat.IsActive {
		t.Error("is_active = false, want true by default")
	}
}

// The key names code. A caller working from an older catalogue has no way to
// discover that one was withdrawn except from this response.
func TestCreateAgentSeatRejectsUnknownKey(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "Unknown")

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"no-such-agent"}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", recorder.Code, recorder.Body.String())
	}

	var problem struct {
		Errors []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if len(problem.Errors) == 0 {
		t.Fatalf("no field errors; body %s", recorder.Body.String())
	}
	if problem.Errors[0].Field != "agent_key" {
		t.Errorf("field = %q, want %q", problem.Errors[0].Field, "agent_key")
	}
	// The valid keys are the actionable part of the message.
	for _, key := range engine.AgentKeys() {
		if !strings.Contains(problem.Errors[0].Message, key) {
			t.Errorf("message %q does not offer the valid key %q", problem.Errors[0].Message, key)
		}
	}
}

func TestCreateAgentSeatRejectsMissingKey(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "MissingKey")

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents", `{}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422; body %s", recorder.Code, recorder.Body.String())
	}
}

// A key is normalised before it is stored, so that one agent cannot enter the
// database under two spellings.
func TestCreateAgentSeatNormalisesTheKey(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "Normalise")
	first := engine.Agents()[0]

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"  `+strings.ToUpper(first.Key)+`  "}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}
	var seat seatResponse
	decodeData(t, recorder, &seat)
	if seat.AgentKey != first.Key {
		t.Errorf("agent_key = %q, want the normalised %q", seat.AgentKey, first.Key)
	}
}

func TestCreateAgentSeatRefusesInactiveGame(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "Closed")

	recorder := do(t, srv, cookie, http.MethodPatch,
		"/api/v1/admin/games/"+itoa(gameID), `{"is_active":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("deactivate game: status %d, body %s", recorder.Code, recorder.Body.String())
	}

	recorder = do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"`+engine.Agents()[0].Key+`"}`)
	if recorder.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body %s", recorder.Code, recorder.Body.String())
	}
}

// The key is what the engine dispatches on and is written into a game's state.
// Accepting it on an update would hand a faction to different code mid-game, so
// the decoder rejects the field outright rather than ignoring it.
func TestUpdateAgentSeatRefusesTheKey(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "Immutable")
	first := engine.Agents()[0]

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"`+first.Key+`"}`)
	var seat seatResponse
	decodeData(t, recorder, &seat)

	recorder = do(t, srv, cookie, http.MethodPatch,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents/"+itoa(seat.PlayerID),
		`{"agent_key":"`+first.Key+`"}`)
	if recorder.Code == http.StatusOK {
		t.Fatal("agent_key was accepted on an update")
	}
	if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 400 or 422; body %s", recorder.Code, recorder.Body.String())
	}
}

func TestUpdateAgentSeat(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "Renaming")

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"`+engine.Agents()[0].Key+`"}`)
	var seat seatResponse
	decodeData(t, recorder, &seat)

	recorder = do(t, srv, cookie, http.MethodPatch,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents/"+itoa(seat.PlayerID),
		`{"agent_name":"Renamed","is_active":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	var updated seatResponse
	decodeData(t, recorder, &updated)
	if updated.AgentName != "Renamed" {
		t.Errorf("agent_name = %q, want %q", updated.AgentName, "Renamed")
	}
	if updated.IsActive {
		t.Error("is_active = true, want false")
	}
	if updated.PlayerID != seat.PlayerID {
		t.Errorf("player_id changed from %d to %d", seat.PlayerID, updated.PlayerID)
	}
}

func TestUpdateAgentSeatRejectsEmptyBody(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "EmptyPatch")

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"`+engine.Agents()[0].Key+`"}`)
	var seat seatResponse
	decodeData(t, recorder, &seat)

	recorder = do(t, srv, cookie, http.MethodPatch,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents/"+itoa(seat.PlayerID), `{}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422; body %s", recorder.Code, recorder.Body.String())
	}
}

// A seat id from one game must not reach another's seat, or every path-scoped
// endpoint is lying about what it operates on.
func TestAgentSeatIsScopedToItsGame(t *testing.T) {
	srv, cookie, _ := testServer(t)
	mine := createGame(t, srv, cookie, "Mine")
	yours := createGame(t, srv, cookie, "Yours")

	recorder := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(mine)+"/agents",
		`{"agent_key":"`+engine.Agents()[0].Key+`"}`)
	var seat seatResponse
	decodeData(t, recorder, &seat)

	recorder = do(t, srv, cookie, http.MethodPatch,
		"/api/v1/admin/games/"+itoa(yours)+"/agents/"+itoa(seat.PlayerID),
		`{"is_active":false}`)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body %s", recorder.Code, recorder.Body.String())
	}
}

// Seats and memberships are separate listings. A bot in the roster would be
// indistinguishable from a person.
func TestAgentSeatsAndMembershipsStaySeparate(t *testing.T) {
	srv, cookie, db := testServer(t)
	gameID := createGame(t, srv, cookie, "Separate")
	ctx := context.Background()

	seated := do(t, srv, cookie, http.MethodPost,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents",
		`{"agent_key":"`+engine.Agents()[0].Key+`"}`)
	if seated.Code != http.StatusCreated {
		t.Fatalf("seat agent: status %d, body %s", seated.Code, seated.Body.String())
	}
	account, err := db.AccountByEmail(ctx, "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	recorder := do(t, srv, cookie, http.MethodPut,
		"/api/v1/admin/games/"+itoa(gameID)+"/memberships/"+itoa(account.ID),
		`{"is_gm":false,"is_active":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save membership: status %d, body %s", recorder.Code, recorder.Body.String())
	}

	recorder = do(t, srv, cookie, http.MethodGet,
		"/api/v1/admin/games/"+itoa(gameID)+"/memberships", "")
	var memberships []struct {
		Email string `json:"email"`
	}
	decodeData(t, recorder, &memberships)
	if len(memberships) != 1 || memberships[0].Email != "user1@example.com" {
		t.Errorf("roster = %+v, want exactly the human seat", memberships)
	}

	recorder = do(t, srv, cookie, http.MethodGet,
		"/api/v1/admin/games/"+itoa(gameID)+"/agents", "")
	var seats []seatResponse
	decodeData(t, recorder, &seats)
	if len(seats) != 1 {
		t.Fatalf("agent listing has %d entries, want 1: %+v", len(seats), seats)
	}
	if seats[0].AgentKey == "" {
		t.Error("agent listing entry has no key")
	}
}

func TestAgentEndpointsRequireAdmin(t *testing.T) {
	srv, cookie, _ := testServer(t)
	gameID := createGame(t, srv, cookie, "Guarded")
	base := "/api/v1/admin/games/" + itoa(gameID) + "/agents"

	cases := []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPost, base, `{"agent_key":"passive"}`},
		{http.MethodPatch, base + "/1", `{"is_active":false}`},
	}
	for _, testCase := range cases {
		recorder := do(t, srv, nil, testCase.method, testCase.path, testCase.body)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous: status = %d, want 401",
				testCase.method, testCase.path, recorder.Code)
		}
	}
}

// itoa keeps path building readable at the call sites.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
