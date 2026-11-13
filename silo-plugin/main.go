// Command siloscope-silo-plugin is a Silo plugin that adds a "Siloscope" entry
// under Plugin Apps in Silo's admin sidebar.
//
// Silo's plugin-content proxy refuses to relay a redirect to another origin,
// so the route answers with a small page that forwards the browser to the
// configured Siloscope URL instead.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	goruntime "runtime"
	"strings"
	"sync"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

// Config keys, matching global_config_schema in manifest.json.
const (
	configKey = "siloscope"
	urlField  = "url"
)

type plugin struct {
	pluginv1.UnimplementedRuntimeServer
	pluginv1.UnimplementedHttpRoutesServer

	manifest *pluginv1.PluginManifest

	mu      sync.RWMutex
	url     string // valid target, or empty
	invalid string // the rejected setting, shown on the setup page
}

func (p *plugin) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: p.manifest}, nil
}

// Configure stores the Siloscope URL. Silo saves the setting without checking
// it, so an invalid URL is not an error here: failing Configure would make
// Silo answer the sidebar link with a bare 502. Instead the URL is cleared
// and the route explains what to fix.
func (p *plugin) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	raw := ""
	for _, entry := range req.GetConfig() {
		if entry.GetKey() == configKey {
			raw = entry.GetValue().GetFields()[urlField].GetStringValue()
		}
	}
	target, ok := parseTarget(raw)
	p.mu.Lock()
	p.url, p.invalid = target, ""
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

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Siloscope</title>
{{if .URL}}<meta http-equiv="refresh" content="0;url={{.URL}}">{{end}}
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; font: 16px/1.5 system-ui, sans-serif; background: Canvas; color: CanvasText; }
  main { max-width: 32rem; padding: 1.5rem; text-align: center; }
  a { color: inherit; font-weight: 600; }
</style>
</head>
<body>
<main>
{{if .URL}}
  <p>Opening Siloscope…</p>
  <p><a href="{{.URL}}">Open Siloscope</a></p>
  <script>location.replace({{.URL}});</script>
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

func (p *plugin) Handle(_ context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if req.GetMethod() != http.MethodGet && req.GetMethod() != http.MethodHead {
		return &pluginv1.HandleHTTPResponse{StatusCode: http.StatusMethodNotAllowed}, nil
	}
	p.mu.RLock()
	data := struct{ URL, Invalid string }{p.url, p.invalid}
	p.mu.RUnlock()

	var body bytes.Buffer
	if err := pageTemplate.Execute(&body, data); err != nil {
		return nil, fmt.Errorf("render page: %w", err)
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: http.StatusOK,
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
