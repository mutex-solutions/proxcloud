package sso

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// keycloakUsers fakes the token endpoint and the admin users API; existing is the id an email search
// finds before any create.
func keycloakUsers(t *testing.T, existing string, createStatus int) (*KCAdmin, *map[string]any) {
	t.Helper()
	created := map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/r/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"tok","expires_in":300}`)
	})
	mux.HandleFunc("GET /admin/realms/r/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("email") != "a@example.com" || r.URL.Query().Get("exact") != "true" {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		id := existing
		if len(created) > 0 {
			id = "new-id"
		}
		if id == "" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `[{"id":%q}]`, id)
	})
	mux.HandleFunc("POST /admin/realms/r/users", func(w http.ResponseWriter, r *http.Request) {
		if createStatus >= 300 {
			w.WriteHeader(createStatus)
			return
		}
		json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(createStatus)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	k, err := NewKCAdmin(srv.URL+"/realms/r", "", "c", "s")
	if err != nil {
		t.Fatal(err)
	}
	return k, &created
}

func TestCreateUser(t *testing.T) {
	k, created := keycloakUsers(t, "", http.StatusCreated)
	id, pw, err := k.CreateUser(context.Background(), "a@example.com", "A", "B")
	if err != nil || id != "new-id" || pw == "" {
		t.Fatalf("id=%q pw=%q err=%v", id, pw, err)
	}
	cred := (*created)["credentials"].([]any)[0].(map[string]any)
	if (*created)["emailVerified"] != true || (*created)["enabled"] != true || cred["value"] != pw || cred["temporary"] != true {
		t.Fatalf("account = %v", *created)
	}
}

func TestCreateUserReusesExistingAccount(t *testing.T) {
	k, created := keycloakUsers(t, "old-id", http.StatusCreated)
	id, pw, err := k.CreateUser(context.Background(), "a@example.com", "", "")
	if err != nil || id != "old-id" || pw != "" || len(*created) != 0 {
		t.Fatalf("id=%q pw=%q err=%v created=%v", id, pw, err, *created)
	}
}

func TestCreateUserRefused(t *testing.T) {
	k, _ := keycloakUsers(t, "", http.StatusForbidden)
	if _, _, err := k.CreateUser(context.Background(), "a@example.com", "", ""); err == nil {
		t.Fatal("a refused create must fail")
	}
}
