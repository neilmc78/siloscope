package main

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignInAdmitsOnlySiloAdministrators(t *testing.T) {
	var siloLogouts atomic.Int32
	problem := func(w http.ResponseWriter, status int, id string) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://siloserver.org/docs/api/v2/problems/" + id, "status": status})
	}
	tokens := func(w http.ResponseWriter, id, name, role string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "silo-" + name, "user": map[string]any{"id": id, "username": name, "role": role}})
	}
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/v2/auth/login":
			switch {
			case body["password"] != "right":
				problem(w, http.StatusUnauthorized, "invalid_credentials")
			case body["username"] == "neil":
				tokens(w, "1", "neil", "admin")
			case body["username"] == "kid":
				tokens(w, "2", "kid", "user")
			case body["username"] == "careful":
				w.Header().Set("Silo-Second-Factor-Challenge", "chal-1")
				w.Header().Set("Silo-Second-Factor-Methods", "totp,passkey,recovery")
				problem(w, http.StatusUnauthorized, "second_factor_required")
			}
		case "/api/v2/auth/login/second-factor":
			if body["challenge_id"] == "chal-1" && body["code"] == "123456" {
				tokens(w, "3", "careful", "admin")
				return
			}
			problem(w, http.StatusUnauthorized, "second_factor_invalid")
		case "/api/v2/auth/logout":
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer silo-") {
				siloLogouts.Add(1)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer silo.Close()
	siloURL, _ := url.Parse(silo.URL)
	t.Setenv("AUTH_MODE", "")
	app := newApplication(config{siloURL: siloURL, apiKey: "key", timeout: 5 * time.Second, cacheTTL: time.Second})
	server := httptest.NewServer(app.routes())
	defer server.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) *http.Response {
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response
	}
	post := func(path, body string, headers ...string) (*http.Response, map[string]any) {
		request, _ := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		for index := 0; index+1 < len(headers); index += 2 {
			request.Header.Set(headers[index], headers[index+1])
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var payload map[string]any
		_ = json.NewDecoder(response.Body).Decode(&payload)
		return response, payload
	}

	// Signed out: the API refuses, pages send you to sign in, the sign-in page and its assets load.
	if code := get("/api/activity").StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("API while signed out = %d", code)
	}
	if response := get("/"); response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/login" {
		t.Fatalf("page while signed out = %d %q", response.StatusCode, response.Header.Get("Location"))
	}
	for _, path := range []string{"/login", "/login.js", "/styles.css", "/preferences.js", "/healthz"} {
		if code := get(path).StatusCode; code != http.StatusOK {
			t.Fatalf("%s while signed out = %d", path, code)
		}
	}

	// A wrong password and a non-admin account are refused without a session.
	if response, _ := post("/api/auth/login", `{"username":"neil","password":"wrong"}`); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", response.StatusCode)
	}
	if response, payload := post("/api/auth/login", `{"username":"kid","password":"right"}`); response.StatusCode != http.StatusForbidden || !strings.Contains(payload["error"].(string), "administrators") {
		t.Fatalf("non-admin = %d %v", response.StatusCode, payload)
	}
	if code := get("/api/activity").StatusCode; code != http.StatusUnauthorized {
		t.Fatal("a refused sign-in left a session")
	}

	// The admin signs in; Siloscope signs out of the Silo session it opened.
	if response, payload := post("/api/auth/login", `{"username":"neil","password":"right"}`); response.StatusCode != http.StatusOK || payload["signed_in"] != true {
		t.Fatalf("admin sign-in = %d %v", response.StatusCode, payload)
	}
	if code := get("/api/activity").StatusCode; code != http.StatusOK {
		t.Fatalf("API after sign-in = %d", code)
	}
	if siloLogouts.Load() != 2 { // the refused non-admin's Silo session was closed too
		t.Fatalf("Silo sessions closed = %d, want 2", siloLogouts.Load())
	}

	// A state-changing request from another site is refused even with the cookie.
	if response, _ := post("/api/people", `{"action":"rename","person_id":1,"display_name":"x"}`, "Origin", "https://evil.example"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d", response.StatusCode)
	}

	// Signing out ends the session.
	if response, _ := post("/api/auth/logout", `{}`); response.StatusCode != http.StatusOK {
		t.Fatalf("sign out = %d", response.StatusCode)
	}
	if code := get("/api/activity").StatusCode; code != http.StatusUnauthorized {
		t.Fatal("session still valid after sign out")
	}

	// Two-step sign-in: the password step returns a challenge, the code completes it.
	response, payload := post("/api/auth/login", `{"username":"careful","password":"right"}`)
	if response.StatusCode != http.StatusOK || payload["second_factor"] != true || payload["challenge"] != "chal-1" {
		t.Fatalf("two-step password step = %d %v", response.StatusCode, payload)
	}
	if methods, _ := payload["methods"].([]any); len(methods) != 2 {
		t.Fatalf("methods = %v, want totp and recovery (no passkey)", payload["methods"])
	}
	if response, _ := post("/api/auth/second-factor", `{"challenge":"chal-1","code":"000000"}`); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code = %d", response.StatusCode)
	}
	if response, _ := post("/api/auth/second-factor", `{"challenge":"chal-1","code":"123 456"}`); response.StatusCode != http.StatusOK {
		t.Fatalf("right code = %d", response.StatusCode)
	}
	if code := get("/api/activity").StatusCode; code != http.StatusOK {
		t.Fatal("two-step sign-in did not open a session")
	}

	// A revoked admin's sessions end.
	app.auth.endUser("3")
	if code := get("/api/activity").StatusCode; code != http.StatusUnauthorized {
		t.Fatal("session survived its account losing admin")
	}

	// Attempts are limited per address.
	limited := false
	for range 12 {
		if response, _ := post("/api/auth/login", `{"username":"neil","password":"wrong"}`); response.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("sign-in attempts were not limited")
	}
}
