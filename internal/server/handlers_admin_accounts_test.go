// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Creating an account with a password is the one path in this service where an
// account's first credential is not chosen by its owner, so these tests are
// about the seam between the two modes: that supplying a password really does
// produce an account somebody can sign in to, that omitting one still produces
// an invitation and nothing else, and that neither mode leaks into the other.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdhender/ecv8-api/internal/store"
)

// createdAccount is the response POST /admin/accounts returns, in the shape a
// caller branches on.
type createdAccount struct {
	Account struct {
		ID        int64  `json:"id"`
		Email     string `json:"email"`
		Role      string `json:"role"`
		IsActive  bool   `json:"is_active"`
		Activated bool   `json:"activated"`
		// ActivationPending reports an unredeemed link, which an activated
		// account must never have.
		ActivationPending bool `json:"activation_pending"`
	} `json:"account"`
	ActivationLink *struct {
		URL string `json:"url"`
	} `json:"activation_link"`
}

func createAccount(t *testing.T, srv *Server, admin *http.Cookie, body string) createdAccount {
	t.Helper()
	recorder := do(t, srv, admin, http.MethodPost, "/api/v1/admin/accounts", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create account: status %d, want 201; body %s", recorder.Code, recorder.Body.String())
	}
	var created createdAccount
	decodeData(t, recorder, &created)
	return created
}

// The whole point of the password mode: one request, and the account can sign
// in. Signing in is what the test asserts, rather than the flags in the
// response, because those could be right while the stored hash was not.
func TestCreateAccountWithAPasswordCanSignInImmediately(t *testing.T) {
	srv, admin, _ := testServer(t)

	created := createAccount(t, srv, admin,
		`{"email":"fixture@example.com","password":"happy"}`)
	if !created.Account.Activated {
		t.Error("activated = false for an account created with a password")
	}
	if !created.Account.IsActive {
		t.Error("is_active = false without being asked for")
	}
	// An activated account has nothing to redeem, so no link is minted for it.
	// A live link nobody asked for would be a credential left lying around.
	if created.ActivationLink != nil {
		t.Errorf("activation_link = %+v, want null alongside a password", created.ActivationLink)
	}
	if created.Account.ActivationPending {
		t.Error("activation_pending = true; a link was written after all")
	}

	recorder := do(t, srv, nil, http.MethodPost, "/api/v1/session",
		`{"email":"fixture@example.com","password":"happy"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("sign in: status %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
}

// Omitting the password must leave the invitation path exactly as it was: the
// account cannot sign in, and the one-time link is returned.
func TestCreateAccountWithoutAPasswordStillInvites(t *testing.T) {
	srv, admin, _ := testServer(t)

	created := createAccount(t, srv, admin, `{"email":"invited@example.com"}`)
	if created.Account.Activated {
		t.Error("activated = true for an invited account")
	}
	if created.ActivationLink == nil || created.ActivationLink.URL == "" {
		t.Fatal("no activation link; an invited account would have no way in")
	}
	if !created.Account.ActivationPending {
		t.Error("activation_pending = false while a link is outstanding")
	}

	// And it genuinely cannot sign in, because it has no password at all.
	recorder := do(t, srv, nil, http.MethodPost, "/api/v1/session",
		`{"email":"invited@example.com","password":"happy"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("sign in: status %d, want 401; body %s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateAccountWithAPasswordHonoursRoleAndActive(t *testing.T) {
	srv, admin, _ := testServer(t)

	created := createAccount(t, srv, admin,
		`{"email":"robot@example.com","password":"happy","role":"admin"}`)
	if created.Account.Role != store.RoleAdmin {
		t.Errorf("role = %q, want %q", created.Account.Role, store.RoleAdmin)
	}

	created = createAccount(t, srv, admin,
		`{"email":"dormant@example.com","password":"happy","is_active":false}`)
	if created.Account.IsActive {
		t.Error("is_active = true when the request asked for false")
	}
	if !created.Account.Activated {
		t.Error("activated = false; an inactive account still has a password")
	}
	// Inactive means no new sessions, whatever the password is.
	recorder := do(t, srv, nil, http.MethodPost, "/api/v1/session",
		`{"email":"dormant@example.com","password":"happy"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("sign in: status %d, want 401; body %s", recorder.Code, recorder.Body.String())
	}
}

// The password goes through the same rule the activation form applies, so this
// endpoint cannot be a way around it.
func TestCreateAccountRejectsAWeakPassword(t *testing.T) {
	srv, admin, _ := testServer(t)

	recorder := do(t, srv, admin, http.MethodPost, "/api/v1/admin/accounts",
		`{"email":"weak@example.com","password":"x"}`)
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
	if len(problem.Errors) == 0 || problem.Errors[0].Field != "password" {
		t.Errorf("errors = %+v, want one naming password", problem.Errors)
	}

	// And nothing was written: a rejected password must not leave a half-made
	// account behind for the next attempt to collide with.
	recorder = do(t, srv, admin, http.MethodGet, "/api/v1/admin/accounts?q=weak@example.com", "")
	var accounts []struct {
		Email string `json:"email"`
	}
	decodeData(t, recorder, &accounts)
	if len(accounts) != 0 {
		t.Errorf("a rejected password still created %+v", accounts)
	}
}

// The endpoint is the administrator's, in both modes. Creating an account is
// not something a game master or a player may do at all.
func TestCreateAccountWithAPasswordRequiresAdmin(t *testing.T) {
	srv, _, db := testServer(t)

	cases := []struct {
		name   string
		cookie *http.Cookie
		status int
	}{
		{"a user", signIn(t, db, "user1@example.com"), http.StatusForbidden},
		{"anonymous", nil, http.StatusUnauthorized},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, srv, testCase.cookie, http.MethodPost, "/api/v1/admin/accounts",
				`{"email":"sneaky@example.com","password":"happy"}`)
			if recorder.Code != testCase.status {
				t.Errorf("status = %d, want %d; body %s",
					recorder.Code, testCase.status, recorder.Body.String())
			}
		})
	}
}

// Only the hash is ever stored, in this path as in every other.
func TestCreateAccountNeverStoresThePlaintext(t *testing.T) {
	srv, admin, db := testServer(t)

	createAccount(t, srv, admin, `{"email":"hashed@example.com","password":"happy"}`)

	account, err := db.AccountByEmail(context.Background(), "hashed@example.com")
	if err != nil {
		t.Fatalf("load the new account: %v", err)
	}
	// The hash is unexported, so the only handle on it is the comparison the
	// rest of the service uses — which is the point: nothing can read it back.
	if err := account.VerifyPassword("happy"); err != nil {
		t.Errorf("the stored credential does not verify the password it was given: %v", err)
	}
	if err := account.VerifyPassword("wrong"); err == nil {
		t.Error("the stored credential verifies a password it was never given")
	}
}
