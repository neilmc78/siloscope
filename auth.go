package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Siloscope sign-in.
//
// Siloscope has no accounts of its own. The sign-in page sends the username
// and password to Silo's own login API (and the second step, for accounts
// with two-step sign-in). Only an administrator account is admitted. Siloscope
// then signs out of Silo straight away, so no Silo session is left behind,
// and issues its own session cookie. Every 15 minutes it re-checks each
// signed-in account with Silo and ends the sessions of any account that was
// disabled, deleted or is no longer an administrator.

const (
	sessionCookie   = "siloscope_session"
	sessionLifetime = 30 * 24 * time.Hour
)

type authSession struct {
	SiloUserID string
	Username   string
	Expires    time.Time
	LastSeen   time.Time
}

type authService struct {
	app      *application
	enabled  bool
	mu       sync.Mutex
	sessions map[string]*authSession // by token hash
	trusted  []netip.Prefix          // reverse proxies whose X-Forwarded-* headers are believed

	// Sign-in budgets. byAddress bounds one client; byName bounds guesses at
	// one account from anywhere, and stays below Silo's own per-name lock
	// (10 in 15 minutes) so Siloscope can never be used to lock an account
	// out of Silo.
	byAddress *attemptLimiter
	byName    *attemptLimiter

	launch *launchService
}

// defaultTrustedProxies trusts only this machine: nginx or Tailscale serve
// running beside Siloscope.
const defaultTrustedProxies = "127.0.0.0/8,::1/128"

func newAuthService(app *application) *authService {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("AUTH_MODE")))
	service := &authService{app: app, enabled: mode != "none", sessions: map[string]*authSession{},
		byAddress: newAttemptLimiter(20, 10*time.Minute), byName: newAttemptLimiter(5, 15*time.Minute)}
	if mode != "" && mode != "none" && mode != "silo" {
		log.Fatal("AUTH_MODE must be silo or none")
	}
	trusted := os.Getenv("TRUSTED_PROXIES")
	if strings.TrimSpace(trusted) == "" {
		trusted = defaultTrustedProxies
	}
	prefixes, err := parsePrefixes(trusted)
	if err != nil {
		log.Fatalf("TRUSTED_PROXIES: %v", err)
	}
	service.trusted = prefixes
	service.launch = newLaunchService(app)
	if !service.enabled {
		log.Print("AUTH_MODE=none: Siloscope has no sign-in; keep it behind an authenticating proxy")
	}
	return service
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// load restores sessions from the database so a restart does not sign everyone out.
func (auth *authService) load(ctx context.Context) {
	if auth.app.store == nil {
		return
	}
	rows, err := auth.app.store.db.QueryContext(ctx, `SELECT token_hash, silo_user_id, username, expires, last_seen FROM auth_sessions WHERE expires > ?`, time.Now().UnixMilli())
	if err != nil {
		return
	}
	defer rows.Close()
	auth.mu.Lock()
	defer auth.mu.Unlock()
	for rows.Next() {
		var hash string
		var session authSession
		var expires, lastSeen int64
		if rows.Scan(&hash, &session.SiloUserID, &session.Username, &expires, &lastSeen) == nil {
			session.Expires, session.LastSeen = time.UnixMilli(expires), time.UnixMilli(lastSeen)
			auth.sessions[hash] = &session
		}
	}
}

func (auth *authService) persist(hash string, session *authSession) {
	if auth.app.store == nil {
		return
	}
	if session == nil {
		_, _ = auth.app.store.db.Exec(`DELETE FROM auth_sessions WHERE token_hash = ?`, hash)
		return
	}
	_, _ = auth.app.store.db.Exec(`INSERT INTO auth_sessions (token_hash, silo_user_id, username, expires, last_seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(token_hash) DO UPDATE SET expires = excluded.expires, last_seen = excluded.last_seen`,
		hash, session.SiloUserID, session.Username, session.Expires.UnixMilli(), session.LastSeen.UnixMilli())
}

// session returns the signed-in session for a request, extending it when used.
func (auth *authService) session(request *http.Request) *authSession {
	cookie, err := request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return nil
	}
	hash := hashToken(cookie.Value)
	auth.mu.Lock()
	session := auth.sessions[hash]
	now := time.Now()
	if session != nil && now.After(session.Expires) {
		delete(auth.sessions, hash)
		session = nil
	}
	extend := session != nil && now.Sub(session.LastSeen) > time.Hour
	if extend {
		session.LastSeen = now
		session.Expires = now.Add(sessionLifetime)
	}
	var copy *authSession
	if session != nil {
		value := *session
		copy = &value
	}
	auth.mu.Unlock()
	if extend {
		auth.persist(hash, copy)
	}
	return copy
}

func secureRequest(request *http.Request) bool {
	return request.TLS != nil || strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https")
}

func (auth *authService) start(writer http.ResponseWriter, request *http.Request, userID, username string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	session := &authSession{SiloUserID: userID, Username: username, Expires: now.Add(sessionLifetime), LastSeen: now}
	hash := hashToken(token)
	auth.mu.Lock()
	auth.sessions[hash] = session
	auth.mu.Unlock()
	auth.persist(hash, session)
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionLifetime.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(request)})
	return nil
}

// parsePrefixes reads a comma-separated list of CIDRs or single addresses.
func parsePrefixes(list string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			address, err := netip.ParseAddr(item)
			if err != nil {
				return nil, fmt.Errorf("%q is not an address or CIDR", item)
			}
			address = address.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or CIDR", item)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func (auth *authService) isTrusted(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range auth.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func peerAddress(request *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	address, err := netip.ParseAddr(host)
	return address.Unmap(), err == nil
}

// fromTrustedProxy reports whether the request arrived through a proxy
// listed in TRUSTED_PROXIES, so its X-Forwarded-* headers can be believed.
func (auth *authService) fromTrustedProxy(request *http.Request) bool {
	peer, ok := peerAddress(request)
	return ok && auth.isTrusted(peer)
}

// clientAddress is the visitor's address. Behind a trusted proxy it walks
// X-Forwarded-For from the right, skipping trusted hops, so a client cannot
// pick its own address by sending the header itself.
func (auth *authService) clientAddress(request *http.Request) string {
	peer, ok := peerAddress(request)
	if !ok {
		return request.RemoteAddr
	}
	if !auth.isTrusted(peer) {
		return peer.String()
	}
	var hops []string
	for _, header := range request.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(header, ",")...)
	}
	for index := len(hops) - 1; index >= 0; index-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[index]))
		if err != nil {
			break
		}
		if hop = hop.Unmap(); !auth.isTrusted(hop) {
			return hop.String()
		}
	}
	return peer.String()
}

// signInName is the per-name budget key: names match Silo accounts trimmed
// and without regard to case, as Silo counts them.
func signInName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

const tooManyAttempts = "Too many sign-in attempts. Wait a few minutes and try again."

// attemptLimiter allows max attempts per key in a sliding window.
type attemptLimiter struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	hits   map[string][]time.Time
	swept  time.Time
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, hits: map[string][]time.Time{}}
}

// allow counts one attempt for key and reports whether it is within budget.
func (limiter *attemptLimiter) allow(key string) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-limiter.window)
	// Drop idle keys now and then, so made-up names cannot grow the map forever.
	if now.Sub(limiter.swept) > time.Minute {
		for other, hits := range limiter.hits {
			if len(hits) == 0 || !hits[len(hits)-1].After(cutoff) {
				delete(limiter.hits, other)
			}
		}
		limiter.swept = now
	}
	recent := limiter.hits[key][:0]
	for _, at := range limiter.hits[key] {
		if at.After(cutoff) {
			recent = append(recent, at)
		}
	}
	if len(recent) >= limiter.max {
		limiter.hits[key] = recent
		return false
	}
	limiter.hits[key] = append(recent, now)
	return true
}

// reset clears key's count after a successful sign-in.
func (limiter *attemptLimiter) reset(key string) {
	limiter.mu.Lock()
	delete(limiter.hits, key)
	limiter.mu.Unlock()
}

type siloLoginResult struct {
	AccessToken string `json:"access_token"`
	User        struct {
		ID                     string `json:"id"`
		Username               string `json:"username"`
		Role                   string `json:"role"`
		PasswordChangeRequired bool   `json:"password_change_required"`
	} `json:"user"`
}

// siloAuth posts to one of Silo's sign-in operations without any credential
// of Siloscope's own.
//
// client is the visitor's address, passed on as X-Forwarded-For so Silo's
// own per-address limits see the visitor rather than Siloscope.
func (auth *authService) siloAuth(ctx context.Context, path string, body any, client string) (*http.Response, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	upstream := *auth.app.config.siloURL
	upstream.Path = strings.TrimRight(upstream.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if client != "" {
		request.Header.Set("X-Forwarded-For", client)
	}
	httpClient := *auth.app.client
	httpClient.Timeout = 15 * time.Second
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response, raw, err
}

// signOutOfSilo ends the Silo session that the sign-in check opened.
func (auth *authService) signOutOfSilo(accessToken string) {
	if accessToken == "" {
		return
	}
	upstream := *auth.app.config.siloURL
	upstream.Path = strings.TrimRight(upstream.Path, "/") + "/api/v2/auth/logout"
	request, err := http.NewRequest(http.MethodPost, upstream.String(), nil)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	client := *auth.app.client
	client.Timeout = 10 * time.Second
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
	}
}

func problemID(raw []byte) string {
	var problem struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &problem) != nil {
		return ""
	}
	return problem.Type[strings.LastIndex(problem.Type, "/")+1:]
}

func writeAuthJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// finish turns Silo's answer to a password or second-step request into a
// Siloscope session, a second-step prompt, or a refusal.
//
// nameKey, when set, is the per-name budget to clear once Silo accepts the
// password: a correct password clears the count, as it does in Silo.
func (auth *authService) finish(writer http.ResponseWriter, request *http.Request, response *http.Response, raw []byte, nameKey string) {
	if nameKey != "" && (response.StatusCode == http.StatusOK || problemID(raw) == "second_factor_required") {
		auth.byName.reset(nameKey)
	}
	switch {
	case response.StatusCode == http.StatusOK:
		var result siloLoginResult
		if json.Unmarshal(raw, &result) != nil || result.User.ID == "" {
			writeJSONError(writer, http.StatusBadGateway, "Silo returned an unexpected sign-in response")
			return
		}
		auth.signOutOfSilo(result.AccessToken)
		if result.User.Role != "admin" {
			writeJSONError(writer, http.StatusForbidden, "Only Silo administrators can sign in to Siloscope.")
			return
		}
		if result.User.PasswordChangeRequired {
			writeJSONError(writer, http.StatusForbidden, "Silo needs you to change this account's password. Sign in to Silo, change it, then try again.")
			return
		}
		if err := auth.start(writer, request, result.User.ID, result.User.Username); err != nil {
			writeJSONError(writer, http.StatusInternalServerError, "The session could not be created")
			return
		}
		writeAuthJSON(writer, http.StatusOK, map[string]any{"signed_in": true, "username": result.User.Username})
	case problemID(raw) == "second_factor_required":
		methods := []string{}
		for _, method := range strings.Split(response.Header.Get("Silo-Second-Factor-Methods"), ",") {
			if method = strings.TrimSpace(method); method == "totp" || method == "recovery" {
				methods = append(methods, method)
			}
		}
		writeAuthJSON(writer, http.StatusOK, map[string]any{"second_factor": true, "challenge": response.Header.Get("Silo-Second-Factor-Challenge"), "methods": methods})
	case response.StatusCode == http.StatusTooManyRequests:
		writeJSONError(writer, http.StatusTooManyRequests, tooManyAttempts)
	case problemID(raw) == "second_factor_invalid":
		writeJSONError(writer, http.StatusUnauthorized, "That code didn't work. Check it and try again.")
	case problemID(raw) == "invalid_token":
		writeJSONError(writer, http.StatusUnauthorized, "The sign-in timed out. Start again.")
	case problemID(raw) == "local_login_disabled":
		writeJSONError(writer, http.StatusForbidden, "Password sign-in is turned off on this Silo server.")
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusUnprocessableEntity:
		writeJSONError(writer, http.StatusUnauthorized, "Wrong username or password.")
	case response.StatusCode == http.StatusForbidden:
		writeJSONError(writer, http.StatusForbidden, "This account can't sign in.")
	default:
		writeJSONError(writer, http.StatusBadGateway, "Silo is unavailable. Try again shortly.")
	}
}

func (auth *authService) loginJSON(writer http.ResponseWriter, request *http.Request) {
	client := auth.clientAddress(request)
	if !auth.byAddress.allow(client) {
		writeJSONError(writer, http.StatusTooManyRequests, tooManyAttempts)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096)).Decode(&input) != nil || strings.TrimSpace(input.Username) == "" || input.Password == "" {
		writeJSONError(writer, http.StatusBadRequest, "Enter your username and password.")
		return
	}
	// The name's budget is spent before Silo sees the password, so a locked
	// name never reaches Silo and cannot add to Silo's own per-name count.
	nameKey := signInName(input.Username)
	if !auth.byName.allow(nameKey) {
		writeJSONError(writer, http.StatusTooManyRequests, tooManyAttempts)
		return
	}
	response, raw, err := auth.siloAuth(request.Context(), "/api/v2/auth/login", map[string]string{"username": strings.TrimSpace(input.Username), "password": input.Password}, client)
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, "Silo is unavailable. Try again shortly.")
		return
	}
	auth.finish(writer, request, response, raw, nameKey)
}

func (auth *authService) secondFactorJSON(writer http.ResponseWriter, request *http.Request) {
	// Silo limits codes per account itself; Siloscope adds the address budget.
	client := auth.clientAddress(request)
	if !auth.byAddress.allow(client) {
		writeJSONError(writer, http.StatusTooManyRequests, tooManyAttempts)
		return
	}
	var input struct {
		Challenge    string `json:"challenge"`
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
	}
	if json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096)).Decode(&input) != nil || input.Challenge == "" || (input.Code == "") == (input.RecoveryCode == "") {
		writeJSONError(writer, http.StatusBadRequest, "Enter a code.")
		return
	}
	body := map[string]string{"challenge_id": input.Challenge}
	if input.Code != "" {
		body["code"] = strings.ReplaceAll(strings.TrimSpace(input.Code), " ", "")
	} else {
		body["recovery_code"] = strings.TrimSpace(input.RecoveryCode)
	}
	response, raw, err := auth.siloAuth(request.Context(), "/api/v2/auth/login/second-factor", body, client)
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, "Silo is unavailable. Try again shortly.")
		return
	}
	auth.finish(writer, request, response, raw, "")
}

func (auth *authService) logoutJSON(writer http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(sessionCookie); err == nil {
		hash := hashToken(cookie.Value)
		auth.mu.Lock()
		delete(auth.sessions, hash)
		auth.mu.Unlock()
		auth.persist(hash, nil)
	}
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(request)})
	writeAuthJSON(writer, http.StatusOK, map[string]bool{"signed_in": false})
}

func (auth *authService) statusJSON(writer http.ResponseWriter, request *http.Request) {
	if !auth.enabled {
		writeAuthJSON(writer, http.StatusOK, map[string]any{"required": false, "signed_in": true})
		return
	}
	session := auth.session(request)
	if session == nil {
		writeAuthJSON(writer, http.StatusOK, map[string]any{"required": true, "signed_in": false})
		return
	}
	writeAuthJSON(writer, http.StatusOK, map[string]any{"required": true, "signed_in": true, "username": session.Username})
}

// publicPaths are reachable without signing in: the sign-in page and what it
// loads, the sign-in operations, and the container health check.
func publicPath(path string) bool {
	switch path {
	case "/login", "/login.html", "/login.js", "/styles.css", "/preferences.js", "/site.webmanifest",
		"/apple-touch-icon.png", "/icon-192.png", "/icon-512.png", "/icon-1024.png", "/healthz",
		"/api/auth/login", "/api/auth/second-factor", "/api/auth/status", "/api/auth/logout", launchPath:
		return true
	}
	return strings.HasPrefix(path, "/icons/")
}

// require guards every other route with a session, and refuses state-changing
// requests that come from another site.
func (auth *authService) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// The Silo launch arrives from Silo's origin by design; its signed
		// token is the protection there instead.
		if request.Method != http.MethodGet && request.Method != http.MethodHead && request.URL.Path != launchPath && !auth.sameOrigin(request) {
			writeJSONError(writer, http.StatusForbidden, "Cross-site request refused")
			return
		}
		if !auth.enabled || publicPath(request.URL.Path) || auth.session(request) != nil {
			next.ServeHTTP(writer, request)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/") {
			writeJSONError(writer, http.StatusUnauthorized, "Sign in required")
			return
		}
		target := "/login"
		if request.URL.Path != "/" {
			target += "?next=" + url.QueryEscape(request.URL.RequestURI())
		}
		http.Redirect(writer, request, target, http.StatusSeeOther)
	})
}

// sameOrigin accepts requests without an Origin header (same-origin GETs,
// non-browser clients) and those whose Origin host matches the request host.
// X-Forwarded-Host counts only from a trusted proxy.
func (auth *authService) sameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := request.Host
	if forwarded := request.Header.Get("X-Forwarded-Host"); forwarded != "" && auth.fromTrustedProxy(request) {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return strings.EqualFold(parsed.Host, host)
}

// recheck ends the sessions of accounts that Silo no longer lists as an
// enabled administrator. It runs every 15 minutes.
func (auth *authService) recheck(ctx context.Context) {
	if !auth.enabled {
		return
	}
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		auth.mu.Lock()
		users := map[string]bool{}
		for _, session := range auth.sessions {
			users[session.SiloUserID] = true
		}
		auth.mu.Unlock()
		silo := &siloIngester{app: auth.app}
		for userID := range users {
			body, status, err := silo.request(ctx, http.MethodGet, "/api/v2/admin/users/"+url.PathEscape(userID), nil, auth.app.config.apiKey)
			var user struct {
				Role    string `json:"role"`
				Enabled bool   `json:"enabled"`
			}
			keep := true
			switch {
			case status == http.StatusNotFound:
				keep = false
			case err == nil && json.Unmarshal(body, &user) == nil:
				keep = user.Enabled && user.Role == "admin"
			}
			// A Silo outage keeps sessions; only a definite answer ends them.
			if !keep {
				auth.endUser(userID)
			}
		}
	}
}

func (auth *authService) endUser(userID string) {
	auth.mu.Lock()
	var ended []string
	for hash, session := range auth.sessions {
		if session.SiloUserID == userID {
			delete(auth.sessions, hash)
			ended = append(ended, hash)
		}
	}
	auth.mu.Unlock()
	for _, hash := range ended {
		auth.persist(hash, nil)
	}
	if len(ended) > 0 {
		log.Printf("signed out %d Siloscope session(s) for Silo user %s: no longer an enabled administrator", len(ended), userID)
	}
}
