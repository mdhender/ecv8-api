// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdhender/ecv8-api/internal/engine"
	"github.com/mdhender/ecv8-api/internal/engine/generators"
)

// clusterResponse mirrors the fields the client renders a map from.
type clusterResponse struct {
	GameID        int64  `json:"game_id"`
	Generator     string `json:"generator"`
	StelliumCount int    `json:"stellium_count"`
	Radius        int    `json:"radius"`
	Stelliums     []struct {
		ID int64 `json:"id"`
		X  int   `json:"x"`
		Y  int   `json:"y"`
		Z  int   `json:"z"`
	} `json:"stelliums"`
}

// gameClusterResponse mirrors the three states the page branches on.
type gameClusterResponse struct {
	GameID   int64            `json:"game_id"`
	GameName string           `json:"game_name"`
	IsActive bool             `json:"is_active"`
	IsSetUp  bool             `json:"is_set_up"`
	Cluster  *clusterResponse `json:"cluster"`
	// A pointer, because present and absent are the two answers that decide
	// whether the form is shown at all.
	Options *struct {
		Generators []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"generators"`
		Generator        string `json:"generator"`
		StelliumCount    int    `json:"stellium_count"`
		Radius           int    `json:"radius"`
		MinStelliumCount int    `json:"min_stellium_count"`
		MaxStelliumCount int    `json:"max_stellium_count"`
		MinRadius        int    `json:"min_radius"`
		MaxRadius        int    `json:"max_radius"`
	} `json:"options"`
}

// setUpGame writes a game's initial state through the API, because a cluster is
// drawn from the seed that writes.
func setUpGame(t *testing.T, srv *Server, gm *http.Cookie, gameID int64, body string) {
	t.Helper()
	if body == "" {
		body = `{}`
	}
	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/state", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("set game up: status %d, body %s", recorder.Code, recorder.Body.String())
	}
}

// getCluster reads the cluster page as the holder of cookie sees it.
func getCluster(t *testing.T, srv *Server, cookie *http.Cookie, gameID int64) gameClusterResponse {
	t.Helper()
	recorder := do(t, srv, cookie, http.MethodGet, "/api/v1/games/"+itoa(gameID)+"/cluster", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	var page gameClusterResponse
	decodeData(t, recorder, &page)
	return page
}

// The default settings a game master is offered have to be the engine's own,
// because those are the ones the endpoint writes when nothing is sent. A second
// set anywhere would be wrong the first time the engine changed its mind.
func TestClusterPageOffersTheEngineDefaults(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Unmapped")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	// Before setup there is nothing to draw from, so no form is offered.
	before := getCluster(t, srv, gm, gameID)
	if before.IsSetUp {
		t.Error("is_set_up = true for a game that has not been set up")
	}
	if before.Cluster != nil {
		t.Errorf("cluster = %+v, want null", before.Cluster)
	}
	if before.Options != nil {
		t.Error("options offered for a game that cannot use them")
	}
	if before.GameName != "Unmapped" {
		t.Errorf("game_name = %q, want %q", before.GameName, "Unmapped")
	}

	setUpGame(t, srv, gm, gameID, "")

	after := getCluster(t, srv, gm, gameID)
	if !after.IsSetUp {
		t.Fatal("is_set_up = false after the game was set up")
	}
	if after.Options == nil {
		t.Fatal("options = null for a game that is ready for a cluster")
	}
	if after.Options.Generator != engine.DefaultGeneratorKey {
		t.Errorf("generator = %q, want %q", after.Options.Generator, engine.DefaultGeneratorKey)
	}
	if after.Options.StelliumCount != generators.DefaultStelliumCount {
		t.Errorf("stellium_count = %d, want %d",
			after.Options.StelliumCount, generators.DefaultStelliumCount)
	}
	if after.Options.Radius != generators.DefaultRadius {
		t.Errorf("radius = %d, want %d", after.Options.Radius, generators.DefaultRadius)
	}
	if after.Options.MinRadius != generators.MinRadius || after.Options.MaxRadius != generators.MaxRadius {
		t.Errorf("radius bounds = %d..%d, want %d..%d",
			after.Options.MinRadius, after.Options.MaxRadius,
			generators.MinRadius, generators.MaxRadius)
	}
	if after.Options.MinStelliumCount != generators.MinStelliumCount ||
		after.Options.MaxStelliumCount != generators.MaxStelliumCount {
		t.Errorf("stellium bounds = %d..%d, want %d..%d",
			after.Options.MinStelliumCount, after.Options.MaxStelliumCount,
			generators.MinStelliumCount, generators.MaxStelliumCount)
	}
	if len(after.Options.Generators) != len(engine.Generators()) {
		t.Errorf("offered %d generators, want %d",
			len(after.Options.Generators), len(engine.Generators()))
	}
}

// An empty body is the ordinary request: a game master with no opinion about
// the shape of their map should not have to invent one.
func TestGenerateClusterWithTheDefaults(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Mapped")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")
	setUpGame(t, srv, gm, gameID, "")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster", `{}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}
	var cluster clusterResponse
	decodeData(t, recorder, &cluster)

	if cluster.Generator != engine.DefaultGeneratorKey {
		t.Errorf("generator = %q, want %q", cluster.Generator, engine.DefaultGeneratorKey)
	}
	if cluster.StelliumCount != generators.DefaultStelliumCount {
		t.Errorf("stellium_count = %d, want %d",
			cluster.StelliumCount, generators.DefaultStelliumCount)
	}
	if len(cluster.Stelliums) != generators.DefaultStelliumCount {
		t.Fatalf("returned %d stelliums, want %d",
			len(cluster.Stelliums), generators.DefaultStelliumCount)
	}

	// Every stellium is inside the sphere it was asked for, and no two share a
	// coordinate — the two things the map has to be true of for the draws prng
	// addresses by (x, y, z) to mean anything.
	seen := map[[3]int]bool{}
	for _, s := range cluster.Stelliums {
		if s.ID == 0 {
			t.Fatalf("stellium %+v has no id", s)
		}
		coordinate := [3]int{s.X, s.Y, s.Z}
		if seen[coordinate] {
			t.Errorf("coordinate %v appears twice", coordinate)
		}
		seen[coordinate] = true
		for axis, value := range coordinate {
			if value < -cluster.Radius || value > cluster.Radius {
				t.Errorf("stellium %v is outside radius %d on axis %d", coordinate, cluster.Radius, axis)
			}
		}
	}

	// The page now shows the map instead of the form.
	page := getCluster(t, srv, gm, gameID)
	if page.Cluster == nil {
		t.Fatal("cluster = null after one was generated")
	}
	if len(page.Cluster.Stelliums) != generators.DefaultStelliumCount {
		t.Errorf("page has %d stelliums, want %d",
			len(page.Cluster.Stelliums), generators.DefaultStelliumCount)
	}
	if page.Options != nil {
		t.Error("options still offered for a game that already has a cluster")
	}
}

// A game master who chooses the settings gets exactly those settings back.
func TestGenerateClusterWithChosenSettings(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Chosen")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")
	setUpGame(t, srv, gm, gameID, "")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster",
		`{"generator":"kiss","stellium_count":40,"radius":8}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}
	var cluster clusterResponse
	decodeData(t, recorder, &cluster)

	if cluster.Radius != 8 {
		t.Errorf("radius = %d, want 8", cluster.Radius)
	}
	if cluster.StelliumCount != 40 || len(cluster.Stelliums) != 40 {
		t.Errorf("stellium_count = %d with %d stelliums, want 40 of each",
			cluster.StelliumCount, len(cluster.Stelliums))
	}
}

// The whole reason a cluster is drawn from the game's seed: the same seed and
// the same settings are the same map, on any machine and at any time. Without
// this the parameters stored beside a cluster would record nothing checkable.
func TestClusterIsReproducibleFromTheSeed(t *testing.T) {
	srv, admin, db := testServer(t)
	const seed = `{"seed":{"hi":"12345678901234567890","lo":"7"}}`

	generate := func(name string) []struct {
		ID int64 `json:"id"`
		X  int   `json:"x"`
		Y  int   `json:"y"`
		Z  int   `json:"z"`
	} {
		t.Helper()
		gameID := createGame(t, srv, admin, name)
		seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
		gm := signIn(t, db, "gm1@example.com")
		setUpGame(t, srv, gm, gameID, seed)

		recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster",
			`{"stellium_count":25,"radius":9}`)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("generate for %s: status %d, body %s", name, recorder.Code, recorder.Body.String())
		}
		var cluster clusterResponse
		decodeData(t, recorder, &cluster)
		return cluster.Stelliums
	}

	first, second := generate("Replay A"), generate("Replay B")
	if len(first) != len(second) {
		t.Fatalf("%d stelliums then %d, want the same map twice", len(first), len(second))
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.X != b.X || a.Y != b.Y || a.Z != b.Z {
			t.Errorf("stellium %d = (%d,%d,%d) then (%d,%d,%d); the same seed must give the same map",
				i, a.X, a.Y, a.Z, b.X, b.Y, b.Z)
		}
	}
}

// The map is the ground every turn is resolved on, so a second generation is
// refused rather than quietly replacing it.
func TestGenerateClusterRefusesASecondOne(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Once")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")
	setUpGame(t, srv, gm, gameID, "")

	first := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster", `{}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first generate: status %d, body %s", first.Code, first.Body.String())
	}
	second := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster",
		`{"radius":20}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second generate: status %d, want 409; body %s", second.Code, second.Body.String())
	}

	// The refusal has to have changed nothing.
	page := getCluster(t, srv, gm, gameID)
	if page.Cluster == nil {
		t.Fatal("cluster = null after a refused second generation")
	}
	if page.Cluster.Radius != generators.DefaultRadius {
		t.Errorf("radius = %d, want the original %d",
			page.Cluster.Radius, generators.DefaultRadius)
	}
}

// A cluster is drawn from the seed, so there is nothing to draw with until the
// game has been set up. That is 409 and not 422: the request is fine, the game
// is not ready.
func TestGenerateClusterRefusesAGameThatIsNotSetUp(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Unstarted")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster", `{}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body.String())
	}
}

// Generating a map for a game nobody is playing is a mistake worth reporting
// rather than recording, on the same rule that refuses to set one up.
func TestGenerateClusterRefusesAnInactiveGame(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Closed")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")
	setUpGame(t, srv, gm, gameID, "")

	closed := do(t, srv, admin, http.MethodPatch,
		"/api/v1/admin/games/"+itoa(gameID), `{"is_active":false}`)
	if closed.Code != http.StatusOK {
		t.Fatalf("deactivate game: status %d, body %s", closed.Code, closed.Body.String())
	}

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster", `{}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body.String())
	}
	// The page still loads — a closed game stays readable — but offers no form.
	page := getCluster(t, srv, gm, gameID)
	if page.IsActive {
		t.Error("is_active = true for a deactivated game")
	}
	if page.Options != nil {
		t.Error("options offered for a game that cannot use them")
	}
}

// Every rejected setting is reported against the field that is wrong, so a game
// master correcting a form is told about all of them at once rather than one
// per attempt.
func TestGenerateClusterRejectsInvalidSettings(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Invalid")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")
	setUpGame(t, srv, gm, gameID, "")

	cases := []struct {
		name   string
		body   string
		fields []string
	}{
		{"an unknown generator", `{"generator":"nope"}`, []string{"generator"}},
		{"a radius below the minimum", `{"radius":2}`, []string{"radius"}},
		{"a radius above the maximum", `{"radius":1025}`, []string{"radius"}},
		{"no stelliums", `{"stellium_count":0}`, []string{"stellium_count"}},
		{"more stelliums than one request may ask for", `{"stellium_count":10001}`, []string{"stellium_count"}},
		{"everything at once", `{"generator":"nope","stellium_count":0,"radius":0}`,
			[]string{"generator", "stellium_count", "radius"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, srv, gm, http.MethodPost,
				"/api/v1/games/"+itoa(gameID)+"/cluster", testCase.body)
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
			got := map[string]bool{}
			for _, e := range problem.Errors {
				got[e.Field] = true
			}
			for _, want := range testCase.fields {
				if !got[want] {
					t.Errorf("no error reported against %q; body %s", want, recorder.Body.String())
				}
			}
			if len(problem.Errors) != len(testCase.fields) {
				t.Errorf("%d field errors, want %d; body %s",
					len(problem.Errors), len(testCase.fields), recorder.Body.String())
			}
		})
	}
}

// A sphere holds a finite number of distinct coordinates, so asking for more
// than fit is a request that could never complete. It is answered against
// radius because raising it is the fix.
func TestGenerateClusterRefusesMoreStelliumsThanTheRadiusHolds(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Crowded")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	gm := signIn(t, db, "gm1@example.com")
	setUpGame(t, srv, gm, gameID, "")

	recorder := do(t, srv, gm, http.MethodPost, "/api/v1/games/"+itoa(gameID)+"/cluster",
		`{"stellium_count":1000,"radius":3}`)
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
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "radius" {
		t.Errorf("errors = %+v, want one against radius", problem.Errors)
	}
}

// The seat decides, exactly as it does everywhere under /games: a player at the
// same table may not generate the map, and someone with no seat is not told the
// game exists.
func TestClusterEndpointsAuthoriseOnTheSeat(t *testing.T) {
	srv, admin, db := testServer(t)
	gameID := createGame(t, srv, admin, "Guarded")
	seatAccount(t, srv, admin, db, gameID, "gm1@example.com", true)
	seatAccount(t, srv, admin, db, gameID, "user1@example.com", false)
	gm := signIn(t, db, "gm1@example.com")
	setUpGame(t, srv, gm, gameID, "")

	cases := []struct {
		name   string
		cookie *http.Cookie
		want   int
	}{
		{"a player at the same table", signIn(t, db, "user1@example.com"), http.StatusForbidden},
		{"an account with no seat", signIn(t, db, "user2@example.com"), http.StatusNotFound},
		// An administrator can never hold a seat, so an administrator is not a
		// player and reaches none of this.
		{"an administrator", admin, http.StatusNotFound},
		{"nobody", nil, http.StatusUnauthorized},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			read := do(t, srv, testCase.cookie, http.MethodGet,
				"/api/v1/games/"+itoa(gameID)+"/cluster", "")
			if read.Code != testCase.want {
				t.Errorf("GET status = %d, want %d; body %s", read.Code, testCase.want, read.Body.String())
			}
			write := do(t, srv, testCase.cookie, http.MethodPost,
				"/api/v1/games/"+itoa(gameID)+"/cluster", `{}`)
			if write.Code != testCase.want {
				t.Errorf("POST status = %d, want %d; body %s", write.Code, testCase.want, write.Body.String())
			}
		})
	}

	// None of the refusals may have left a map behind.
	page := getCluster(t, srv, gm, gameID)
	if page.Cluster != nil {
		t.Error("a refused request generated a cluster")
	}
}
