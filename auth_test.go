package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func TestClientAddressBelievesOnlyTrustedProxies(t *testing.T) {
	auth := &authService{}
	auth.trusted, _ = parsePrefixes(defaultTrustedProxies + ",172.18.0.0/16")
	cases := []struct {
		name, peer, forwarded, want string
	}{
		{"direct client ignores its own header", "203.0.113.9:4000", "10.0.0.1", "203.0.113.9"},
		{"local proxy passes the visitor on", "127.0.0.1:5000", "198.51.100.7", "198.51.100.7"},
		{"spoofed left entry is skipped", "127.0.0.1:5000", "1.2.3.4, 198.51.100.7", "198.51.100.7"},
		{"trusted hops are walked past", "127.0.0.1:5000", "198.51.100.7, 172.18.0.5", "198.51.100.7"},
		{"no header falls back to the proxy", "[::1]:5000", "", "::1"},
		{"garbage stops the walk", "127.0.0.1:5000", "nonsense", "127.0.0.1"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = tc.peer
		if tc.forwarded != "" {
			request.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		if got := auth.clientAddress(request); got != tc.want {
			t.Errorf("%s: clientAddress = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := parsePrefixes("10.0.0.0/8, bogus"); err == nil {
		t.Error("a bad TRUSTED_PROXIES entry was accepted")
	}
}

// Siloscope's per-name budget must run out before Silo's (10 in 15 minutes),
// so wrong passwords sent through Siloscope can never lock an account in Silo.
func TestNameBudgetStopsBeforeSiloLocksTheAccount(t *testing.T) {
	var siloAttempts atomic.Int32
	var forwardedFor atomic.Value
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			siloAttempts.Add(1)
			forwardedFor.Store(r.Header.Get("X-Forwarded-For"))
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["password"] == "right" {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "user": map[string]any{"id": "1", "username": "neil", "role": "admin"}})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer silo.Close()
	siloURL, _ := url.Parse(silo.URL)
	t.Setenv("AUTH_MODE", "")
	app := newApplication(config{siloURL: siloURL, apiKey: "key", timeout: 5 * time.Second, cacheTTL: time.Second})
	handler := app.routes()
	attempt := func(name, password, from string) int {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"`+name+`","password":"`+password+`"}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "127.0.0.1:1234" // behind the local proxy
		request.Header.Set("X-Forwarded-For", from)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	// A correct password clears the count, as in Silo.
	for range 4 {
		attempt("neil", "wrong", "198.51.100.1")
	}
	if code := attempt("neil", "right", "198.51.100.1"); code != http.StatusOK {
		t.Fatalf("right password = %d", code)
	}
	if got := forwardedFor.Load(); got != "198.51.100.1" {
		t.Fatalf("Silo saw X-Forwarded-For %v, want the visitor", got)
	}
	// Five wrong guesses from anywhere, under any spelling, reach Silo...
	siloAttempts.Store(0)
	for index, name := range []string{"neil", "NEIL", " Neil ", "neil", "nEiL"} {
		if code := attempt(name, "wrong", "203.0.113."+strconv.Itoa(index+1)); code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d", index+1, code)
		}
	}
	// ...and the sixth is refused without asking Silo, even with the right password.
	if code := attempt("neil", "right", "203.0.113.99"); code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt = %d, want 429", code)
	}
	if got := siloAttempts.Load(); got != 5 {
		t.Fatalf("Silo saw %d attempts, want 5 (Silo locks at 10)", got)
	}
	// Other names are unaffected.
	if code := attempt("someone", "wrong", "203.0.113.50"); code != http.StatusUnauthorized {
		t.Fatalf("other name = %d", code)
	}
}

func launchToken(secret string, claims launchClaims) string {
	raw, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(signLaunch(secret, payload))
}

func TestSiloLaunchOpensSessionOnlyForAVerifiedAdmin(t *testing.T) {
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v2/admin/users/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "username": "neil", "role": "admin", "enabled": true})
		case "/api/v2/admin/users/2":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "2", "username": "kid", "role": "user", "enabled": true})
		case "/api/v2/admin/users/3":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "3", "username": "gone", "role": "admin", "enabled": false})
		case "/api/v2/admin/users/9":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer silo.Close()
	siloURL, _ := url.Parse(silo.URL)
	t.Setenv("AUTH_MODE", "")
	t.Setenv("SILO_LAUNCH_SECRET", "shared-secret")
	app := newApplication(config{siloURL: siloURL, apiKey: "key", timeout: 5 * time.Second, cacheTTL: time.Second})
	server := httptest.NewServer(app.routes())
	defer server.Close()

	now := time.Now()
	claims := func(uid, nonce string) launchClaims {
		return launchClaims{Version: 1, UserID: uid, Name: "x", Issued: now.Unix(), Expires: now.Add(time.Minute).Unix(), Nonce: nonce}
	}
	// launch posts the form the plugin's page sends, from Silo's origin.
	launch := func(token string) (*http.Response, *http.Client) {
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request, _ := http.NewRequest(http.MethodPost, server.URL+launchPath, strings.NewReader(url.Values{"token": {token}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "https://silo.example")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response, client
	}
	refused := func(name, token, reason string) {
		t.Helper()
		response, _ := launch(token)
		if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/login?launch="+reason {
			t.Errorf("%s: %d %q, want refusal %q", name, response.StatusCode, response.Header.Get("Location"), reason)
		}
		for _, cookie := range response.Cookies() {
			if cookie.Name == sessionCookie && cookie.Value != "" {
				t.Errorf("%s: refused launch set a session cookie", name)
			}
		}
	}

	valid := launchToken("shared-secret", claims("1", "nonce-aaaaaaaaaaaaaaaa"))
	response, client := launch(valid)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid launch = %d %q", response.StatusCode, response.Header.Get("Location"))
	}
	if check, _ := client.Get(server.URL + "/api/activity"); check.StatusCode != http.StatusOK {
		t.Fatalf("session after launch = %d", check.StatusCode)
	}

	expired := claims("1", "nonce-bbbbbbbbbbbbbbbb")
	expired.Issued, expired.Expires = now.Add(-3*time.Minute).Unix(), now.Add(-2*time.Minute).Unix()
	tooLong := claims("1", "nonce-cccccccccccccccc")
	tooLong.Expires = now.Add(time.Hour).Unix()
	refused("replayed token", valid, launchExpired)
	refused("wrong secret", launchToken("other-secret", claims("1", "nonce-dddddddddddddddd")), launchBadSecret)
	refused("expired token", launchToken("shared-secret", expired), launchExpired)
	refused("over-long token", launchToken("shared-secret", tooLong), launchExpired)
	refused("tampered payload", "eyJ2IjoxfQ."+strings.SplitN(valid, ".", 2)[1], launchBadSecret)
	refused("not a token", "nonsense", launchInvalid)
	refused("non-admin in Silo", launchToken("shared-secret", claims("2", "nonce-eeeeeeeeeeeeeeee")), launchNotAdmin)
	refused("disabled in Silo", launchToken("shared-secret", claims("3", "nonce-ffffffffffffffff")), launchNotAdmin)
	refused("unknown to Silo", launchToken("shared-secret", claims("8", "nonce-gggggggggggggggg")), launchNotAdmin)
	refused("Silo unavailable", launchToken("shared-secret", claims("9", "nonce-hhhhhhhhhhhhhhhh")), launchSilo)
}
