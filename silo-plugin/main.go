// Command siloscope-silo-plugin is a Silo plugin that adds a "Siloscope" entry
// under Plugin Apps in Silo's admin sidebar.
//
// Silo's plugin-content proxy refuses to relay a redirect to another origin,
// so the route answers with a small page that sends the browser on to the
// configured Siloscope URL instead. With a launch secret shared with
// Siloscope, that page also signs the administrator in to Siloscope, so no
// password is typed (see launch.go in the Siloscope repository).
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

// Config keys, matching global_config_schema in manifest.json.
const (
	configKey   = "siloscope"
	urlField    = "url"
	secretField = "launch_secret"
)

// The launch token format Siloscope verifies.
const (
	launchContext  = "siloscope-launch-v1."
	launchLifetime = 60 * time.Second
	launchPath     = "api/auth/silo-launch"
)

type plugin struct {
	pluginv1.UnimplementedRuntimeServer
	pluginv1.UnimplementedHttpRoutesServer

	manifest *pluginv1.PluginManifest
	now      func() time.Time

	mu      sync.RWMutex
	url     string // valid target, or empty
	invalid string // the rejected setting, shown on the setup page
	secret  string // launch secret shared with Siloscope, or empty
}

func (p *plugin) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: p.manifest}, nil
}

// Configure stores the Siloscope URL and launch secret. Silo saves settings
// without checking them, so an invalid URL is not an error here: failing
// Configure would make Silo answer the sidebar link with a bare 502. Instead
// the URL is cleared and the route explains what to fix.
func (p *plugin) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	raw, secret := "", ""
	for _, entry := range req.GetConfig() {
		if entry.GetKey() == configKey {
			fields := entry.GetValue().GetFields()
			raw = fields[urlField].GetStringValue()
			secret = strings.TrimSpace(fields[secretField].GetStringValue())
		}
	}
	target, ok := parseTarget(raw)
	p.mu.Lock()
	p.url, p.invalid, p.secret = target, "", secret
	if !ok {
		p.invalid = strings.TrimSpace(raw)
	}
	p.mu.Unlock()
	return &pluginv1.ConfigureResponse{}, nil
}

// parseTarget accepts only an absolute http or https URL. The forwarding page
// runs on Silo's origin, so a javascript: or data: URL must never get through.
// ok is false only for a non-empty value that is not such a URL.
func parseTarget(raw string) (target string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || strings.ContainsAny(raw, " \t\r\n\\") {
		return "", false
	}
	return parsed.String(), true
}

type launchClaims struct {
	Version int    `json:"v"`
	UserID  string `json:"uid"`
	Name    string `json:"name"`
	Issued  int64  `json:"iat"`
	Expires int64  `json:"exp"`
	Nonce   string `json:"nonce"`
}

// launchToken signs a single-use, one-minute token naming the Silo user.
func launchToken(secret, userID, name string, now time.Time) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	raw, err := json.Marshal(launchClaims{Version: 1, UserID: userID, Name: name, Issued: now.Unix(), Expires: now.Add(launchLifetime).Unix(), Nonce: hex.EncodeToString(nonce)})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(launchContext + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

type pageData struct {
	URL     string // plain forward
	Action  string // launch form target
	Token   string
	Invalid string
	Refused bool
}

// The page paints in Silo's own colours so the hop never flashes white.
var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Siloscope</title>
{{if .URL}}<meta http-equiv="refresh" content="0;url={{.URL}}">{{end}}
<style>
  :root { color-scheme: dark; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; font: 15px/1.5 "Plus Jakarta Sans", system-ui, sans-serif; background: #141417; color: #e8e8ec; }
  main { max-width: 32rem; padding: 1.5rem; text-align: center; }
  p { margin: 0 0 0.75rem; }
  a { color: inherit; font-weight: 600; }
  code { color: #a4a4ae; word-break: break-all; }
  button { font: inherit; font-weight: 600; color: #141417; background: #e8e8ec; border: 0; border-radius: 8px; padding: 0.5rem 1rem; cursor: pointer; }
</style>
</head>
<body>
<main>
{{if .Action}}
  <form id="launch" method="post" action="{{.Action}}">
    <input type="hidden" name="token" value="{{.Token}}">
    <p>Opening Siloscope…</p>
    <button type="submit">Open Siloscope</button>
  </form>
  <script>document.getElementById("launch").submit();</script>
{{else if .URL}}
  <p>Opening Siloscope…</p>
  <p><a href="{{.URL}}">Open Siloscope</a></p>
  <script>location.replace({{.URL}});</script>
{{else if .Refused}}
  <p>Only Silo administrators can open Siloscope.</p>
{{else if .Invalid}}
  <p>The Siloscope URL <code>{{.Invalid}}</code> isn't a web address starting with http:// or https://.</p>
  <p>Fix it in <a href="/admin/plugins/siloscope.link">Admin → Plugins → Siloscope</a>, then try again.</p>
{{else}}
  <p>Siloscope's address isn't set yet.</p>
  <p>Set it in <a href="/admin/plugins/siloscope.link">Admin → Plugins → Siloscope</a>, then try again.</p>
{{end}}
</main>
</body>
</html>
`))

// header reads a request header whatever its spelling in the map.
func header(req *pluginv1.HandleHTTPRequest, name string) string {
	for key, value := range req.GetHeaders() {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (p *plugin) Handle(_ context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if req.GetMethod() != http.MethodGet && req.GetMethod() != http.MethodHead {
		return &pluginv1.HandleHTTPResponse{StatusCode: http.StatusMethodNotAllowed}, nil
	}
	p.mu.RLock()
	target, invalid, secret := p.url, p.invalid, p.secret
	p.mu.RUnlock()

	data := pageData{Invalid: invalid}
	switch {
	case target == "":
	case secret == "":
		data.URL = target
	default:
		// Silo's proxy drops any X-Silo-* header a browser sends and sets
		// these itself, with the admin role only for an acting administrator.
		userID := header(req, "X-Silo-User-Id")
		if userID == "" || header(req, "X-Silo-User-Role") != "admin" {
			data.Refused = true
			break
		}
		action, err := url.JoinPath(target, launchPath)
		if err != nil {
			return nil, fmt.Errorf("launch url: %w", err)
		}
		now := time.Now
		if p.now != nil {
			now = p.now
		}
		token, err := launchToken(secret, userID, header(req, "X-Silo-User-Name"), now())
		if err != nil {
			return nil, fmt.Errorf("sign launch token: %w", err)
		}
		data.Action, data.Token = action, token
	}

	var body bytes.Buffer
	if err := pageTemplate.Execute(&body, data); err != nil {
		return nil, fmt.Errorf("render page: %w", err)
	}
	status := http.StatusOK
	if data.Refused {
		status = http.StatusForbidden
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers: map[string]string{
			"Content-Type":  "text/html; charset=utf-8",
			"Cache-Control": "no-store",
		},
		Body: body.Bytes(),
	}, nil
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}
	p := &plugin{manifest: manifest}
	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime:    p,
			HttpRoutes: p,
		},
	})
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])
	if len(manifest.GetSupportedPlatforms()) == 0 {
		manifest.SupportedPlatforms = []*pluginv1.SupportedPlatform{{Os: goruntime.GOOS, Arch: goruntime.GOARCH}}
	}
	return manifest, nil
}
