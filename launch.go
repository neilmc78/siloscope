package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// One-click sign-in from Silo.
//
// The Siloscope plugin in Silo knows who opened its sidebar link: Silo's
// plugin proxy strips any X-Silo-* header a browser sends and sets
// X-Silo-User-Id and X-Silo-User-Role itself. For an administrator, the
// plugin signs a short-lived token with a secret it shares with Siloscope
// and has the browser POST it here. Siloscope checks the signature, expiry
// and single use, then asks Silo with its API key that the account is still
// an enabled administrator before opening a session. No password is
// involved, so this also works while a name's password budget is spent.
//
// Token: base64url(payload JSON) "." base64url(HMAC-SHA256(secret,
// launchContext + payload part)). The secret is used as the UTF-8 bytes of
// the string an administrator copies between the two.

const (
	launchPath      = "/api/auth/silo-launch"
	launchContext   = "siloscope-launch-v1."
	launchStateKey  = "launch_secret"
	launchMaxLife   = 2 * time.Minute // longest exp - iat accepted
	launchClockSkew = 2 * time.Minute // how far ahead iat may be
)

type launchClaims struct {
	Version int    `json:"v"`
	UserID  string `json:"uid"`
	Name    string `json:"name"`
	Issued  int64  `json:"iat"`
	Expires int64  `json:"exp"`
	Nonce   string `json:"nonce"`
}

type launchService struct {
	app *application

	mu         sync.Mutex
	secret     string
	fromEnv    bool
	nonces     map[string]time.Time // used nonces, until they expire
	lastLaunch time.Time
	lastUser   string
}

func newLaunchService(app *application) *launchService {
	service := &launchService{app: app, nonces: map[string]time.Time{}}
	if secret := strings.TrimSpace(os.Getenv("SILO_LAUNCH_SECRET")); secret != "" {
		service.secret, service.fromEnv = secret, true
	}
	return service
}

func newLaunchSecret() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

// currentSecret returns the shared secret, loading or creating it on first
// use (the store opens after the auth service is built).
func (launch *launchService) currentSecret() string {
	launch.mu.Lock()
	defer launch.mu.Unlock()
	if launch.secret != "" {
		return launch.secret
	}
	if store := launch.app.store; store != nil {
		launch.secret = store.getState(launchStateKey)
	}
	if launch.secret == "" {
		launch.secret = newLaunchSecret()
		launch.persistLocked()
	}
	return launch.secret
}

func (launch *launchService) persistLocked() {
	if store := launch.app.store; store != nil {
		if err := store.setState(launchStateKey, launch.secret); err != nil {
			log.Printf("could not save the Silo launch secret: %v", err)
		}
	} else {
		log.Print("no data directory: the Silo launch secret changes on every restart")
	}
}

// regenerate replaces the secret, so every plugin holding the old one stops
// working until it is given the new one.
func (launch *launchService) regenerate() (string, error) {
	launch.mu.Lock()
	defer launch.mu.Unlock()
	if launch.fromEnv {
		return "", errors.New("the secret is set by SILO_LAUNCH_SECRET")
	}
	launch.secret = newLaunchSecret()
	launch.persistLocked()
	return launch.secret, nil
}

// Reasons a launch is refused; the sign-in page explains each.
const (
	launchBadSecret = "secret"
	launchExpired   = "expired"
	launchNotAdmin  = "admin"
	launchSilo      = "silo"
	launchInvalid   = "invalid"
)

func signLaunch(secret, payloadPart string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(launchContext + payloadPart))
	return mac.Sum(nil)
}

// verify checks a token's signature, lifetime and single use. It returns the
// Silo user ID it names, or a refusal reason.
func (launch *launchService) verify(token string, now time.Time) (string, string) {
	payloadPart, signaturePart, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || len(token) > 2048 {
		return "", launchInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return "", launchInvalid
	}
	if !hmac.Equal(signature, signLaunch(launch.currentSecret(), payloadPart)) {
		return "", launchBadSecret
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadPart)
	var claims launchClaims
	if err != nil || json.Unmarshal(raw, &claims) != nil || claims.Version != 1 || claims.UserID == "" || len(claims.Nonce) < 16 {
		return "", launchInvalid
	}
	issued, expires := time.Unix(claims.Issued, 0), time.Unix(claims.Expires, 0)
	if !now.Before(expires) || expires.Sub(issued) > launchMaxLife || issued.After(now.Add(launchClockSkew)) {
		return "", launchExpired
	}
	launch.mu.Lock()
	defer launch.mu.Unlock()
	for nonce, until := range launch.nonces {
		if now.After(until) {
			delete(launch.nonces, nonce)
		}
	}
	if _, used := launch.nonces[claims.Nonce]; used {
		return "", launchExpired
	}
	launch.nonces[claims.Nonce] = expires
	return claims.UserID, ""
}

var launchDonePage = template.Must(template.New("launch").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="0;url=/"><title>Siloscope</title>
<link rel="stylesheet" href="/styles.css"><script src="/preferences.js"></script></head>
<body class="auth-body"><main class="auth-shell"><p>Signed in as {{.}}. <a href="/">Open Siloscope</a></p></main></body></html>
`))

// launchJSON handles the browser's POST from the plugin's page. Every answer
// is a navigation: the session on success, the sign-in page with a reason
// otherwise.
func (auth *authService) launchJSON(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	fail := func(reason string) {
		http.Redirect(writer, request, "/login?launch="+url.QueryEscape(reason), http.StatusSeeOther)
	}
	if !auth.enabled {
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	if !auth.byAddress.allow(auth.clientAddress(request)) {
		fail(launchInvalid)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	if request.ParseForm() != nil {
		fail(launchInvalid)
		return
	}
	userID, reason := auth.launch.verify(request.PostForm.Get("token"), time.Now())
	if reason != "" {
		fail(reason)
		return
	}
	// The token proves Silo's plugin vouched for this user a moment ago; Silo
	// itself decides whether the account may still sign in. A Silo outage
	// refuses: unlike the periodic re-check, this opens a new session.
	silo := &siloIngester{app: auth.app}
	body, status, err := silo.request(request.Context(), http.MethodGet, "/api/v2/admin/users/"+url.PathEscape(userID), nil, auth.app.config.apiKey)
	var user struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Enabled  bool   `json:"enabled"`
	}
	switch {
	case status == http.StatusNotFound: // a definite answer: no such account
		fail(launchNotAdmin)
		return
	case err != nil || status != http.StatusOK:
		fail(launchSilo)
		return
	case json.Unmarshal(body, &user) != nil || !user.Enabled || user.Role != "admin":
		fail(launchNotAdmin)
		return
	}
	if err := auth.start(writer, request, userID, user.Username); err != nil {
		fail(launchSilo)
		return
	}
	auth.launch.mu.Lock()
	auth.launch.lastLaunch, auth.launch.lastUser = time.Now(), user.Username
	auth.launch.mu.Unlock()
	// A page that refreshes to "/" rather than a redirect: the browser then
	// arrives from Siloscope itself, so the new SameSite=Lax cookie is sent.
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = launchDonePage.Execute(writer, user.Username)
}

// launchSettingsJSON shows the shared secret on the Settings tab. Only a
// signed-in administrator reaches it (auth.require).
func (auth *authService) launchSettingsJSON(writer http.ResponseWriter, request *http.Request) {
	if !auth.enabled {
		writeAuthJSON(writer, http.StatusOK, map[string]any{"disabled": true})
		return
	}
	launch := auth.launch
	secret := launch.currentSecret()
	launch.mu.Lock()
	payload := map[string]any{"secret": secret, "from_env": launch.fromEnv, "last_user": launch.lastUser}
	if !launch.lastLaunch.IsZero() {
		payload["last_launch"] = launch.lastLaunch.UTC().Format(time.RFC3339)
	}
	launch.mu.Unlock()
	writeAuthJSON(writer, http.StatusOK, payload)
}

func (auth *authService) launchRegenerateJSON(writer http.ResponseWriter, request *http.Request) {
	if !sameOriginSettingsRequest(request) {
		writeJSONError(writer, http.StatusForbidden, "Cross-site request refused")
		return
	}
	secret, err := auth.launch.regenerate()
	if err != nil {
		writeJSONError(writer, http.StatusConflict, "The secret is set by SILO_LAUNCH_SECRET. Change it there.")
		return
	}
	writeAuthJSON(writer, http.StatusOK, map[string]any{"secret": secret})
}
