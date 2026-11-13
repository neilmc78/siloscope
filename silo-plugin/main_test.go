package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/types/known/structpb"
)

func configure(t *testing.T, p *plugin, raw string) {
	t.Helper()
	value, err := structpb.NewStruct(map[string]any{urlField: raw})
	if err != nil {
		t.Fatal(err)
	}
	// Configure must never fail: Silo would turn that into a bare 502.
	if _, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: configKey, Value: value}},
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

func page(t *testing.T, p *plugin) string {
	t.Helper()
	resp, err := p.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatusCode() != 200 || resp.GetHeaders()["Content-Type"] != "text/html; charset=utf-8" {
		t.Fatalf("status %d headers %v", resp.GetStatusCode(), resp.GetHeaders())
	}
	return string(resp.GetBody())
}

func TestForwardingPage(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		refused    bool
		contains   []string
		notContain []string
	}{
		{
			name:     "unset shows setup page",
			url:      "",
			contains: []string{"isn't set yet", `href="/admin/plugins/siloscope.link"`},
		},
		{
			name:     "valid url forwards",
			url:      "http://192.168.1.10:8080/",
			contains: []string{`content="0;url=http://192.168.1.10:8080/"`, `href="http://192.168.1.10:8080/"`, `location.replace("http://192.168.1.10:8080/")`},
		},
		{
			name:       "markup in url is escaped",
			url:        `https://scope.example/?q="</script><script>alert(1)</script>`,
			notContain: []string{"<script>alert(1)", `?q="`},
		},
		// A refused URL is shown as text in the explanation, but never as a
		// link, refresh target or script value.
		{name: "javascript scheme refused", url: "javascript:alert(1)", refused: true},
		{name: "data scheme refused", url: "data:text/html,hi", refused: true},
		{name: "relative url refused", url: "//evil.example/", refused: true},
		{name: "backslash refused", url: `http://ok.example\@evil.example/`, refused: true},
		{name: "markup in refused url is escaped", url: `x"><script>alert(1)</script>`, refused: true, notContain: []string{"<script>alert(1)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from a valid URL so a refused one must clear it.
			p := &plugin{url: "http://previous.example/"}
			configure(t, p, tc.url)
			body := page(t, p)
			if tc.refused {
				if !strings.Contains(body, "isn't a web address") {
					t.Errorf("refused url should explain the problem, got:\n%s", body)
				}
				for _, s := range []string{"http-equiv", "location.replace", "previous.example", "Open Siloscope"} {
					if strings.Contains(body, s) {
						t.Errorf("refused url page contains %q:\n%s", s, body)
					}
				}
			}
			for _, s := range tc.contains {
				if !strings.Contains(body, s) {
					t.Errorf("body missing %q:\n%s", s, body)
				}
			}
			for _, s := range tc.notContain {
				if strings.Contains(body, s) {
					t.Errorf("body contains %q:\n%s", s, body)
				}
			}
		})
	}
}

func TestOnlyGetIsServed(t *testing.T) {
	resp, err := (&plugin{}).Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/"})
	if err != nil || resp.GetStatusCode() != 405 {
		t.Fatalf("POST: status %d err %v", resp.GetStatusCode(), err)
	}
}

func TestManifestLoads(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	routes := manifest.GetHttpRoutes()
	if len(routes) != 1 || routes[0].GetAccess() != "admin" || !routes[0].GetNavigable() || routes[0].GetNavigationKind() != "admin" {
		t.Fatalf("route must be an admin-only admin nav entry: %v", routes)
	}
	if len(manifest.GetGlobalConfigSchema()) != 1 || manifest.GetGlobalConfigSchema()[0].GetKey() != configKey {
		t.Fatalf("config schema key must be %q", configKey)
	}
	// The launch secret must be a secret field, so Silo never shows it back.
	secret := false
	for _, field := range manifest.GetGlobalConfigSchema()[0].GetAdminForm().GetFields() {
		if field.GetKey() == secretField {
			secret = field.GetSecret() && field.GetControl() == pluginv1.AdminFormControl_ADMIN_FORM_CONTROL_PASSWORD
		}
	}
	if !secret {
		t.Fatalf("%s must be a secret password field", secretField)
	}
}

func TestLaunchPage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	configureWith := func(target, secret string) *plugin {
		p := &plugin{now: func() time.Time { return now }}
		value, _ := structpb.NewStruct(map[string]any{urlField: target, secretField: secret})
		if _, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Config: []*pluginv1.ConfigEntry{{Key: configKey, Value: value}}}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	open := func(p *plugin, headers map[string]string) (int32, string) {
		resp, err := p.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/", Headers: headers})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetStatusCode(), string(resp.GetBody())
	}
	admin := map[string]string{"X-Silo-User-Id": "7", "X-Silo-User-Role": "admin", "X-Silo-User-Name": "neil"}
	tokenPattern := regexp.MustCompile(`name="token" value="([^"]+)"`)

	t.Run("admin gets a signed one-minute token", func(t *testing.T) {
		status, body := open(configureWith("https://scope.example:8443/", "shared"), admin)
		if status != 200 || !strings.Contains(body, `action="https://scope.example:8443/api/auth/silo-launch"`) {
			t.Fatalf("status %d body:\n%s", status, body)
		}
		if strings.Contains(body, "http-equiv") || strings.Contains(body, "location.replace") {
			t.Fatalf("launch page also forwards without the token:\n%s", body)
		}
		match := tokenPattern.FindStringSubmatch(body)
		if match == nil {
			t.Fatalf("no token in page:\n%s", body)
		}
		payload, signature, _ := strings.Cut(match[1], ".")
		mac := hmac.New(sha256.New, []byte("shared"))
		mac.Write([]byte(launchContext + payload))
		if got, _ := base64.RawURLEncoding.DecodeString(signature); !hmac.Equal(got, mac.Sum(nil)) {
			t.Fatal("token signature does not verify with the shared secret")
		}
		raw, _ := base64.RawURLEncoding.DecodeString(payload)
		var claims launchClaims
		if err := json.Unmarshal(raw, &claims); err != nil || claims.Version != 1 || claims.UserID != "7" || claims.Expires-claims.Issued != 60 || len(claims.Nonce) < 16 {
			t.Fatalf("claims = %+v (%v)", claims, err)
		}
		// Each opening gets its own nonce, so a token is good once.
		_, again := open(configureWith("https://scope.example:8443/", "shared"), admin)
		if tokenPattern.FindStringSubmatch(again)[1] == match[1] {
			t.Fatal("two launches produced the same token")
		}
	})
	t.Run("a Siloscope under a path keeps it", func(t *testing.T) {
		_, body := open(configureWith("https://host.example/scope", "shared"), admin)
		if !strings.Contains(body, `action="https://host.example/scope/api/auth/silo-launch"`) {
			t.Fatalf("body:\n%s", body)
		}
	})
	t.Run("no token without Silo's admin role", func(t *testing.T) {
		for _, headers := range []map[string]string{
			{"X-Silo-User-Id": "7", "X-Silo-User-Role": "user"},
			{"X-Silo-User-Role": "admin"},
			nil,
		} {
			status, body := open(configureWith("https://scope.example/", "shared"), headers)
			if status != 403 || strings.Contains(body, "token") || strings.Contains(body, "scope.example") {
				t.Fatalf("headers %v: status %d body:\n%s", headers, status, body)
			}
		}
	})
	t.Run("without a secret the link only forwards", func(t *testing.T) {
		_, body := open(configureWith("https://scope.example/", ""), admin)
		if strings.Contains(body, "silo-launch") || !strings.Contains(body, `location.replace("https://scope.example/")`) {
			t.Fatalf("body:\n%s", body)
		}
	})
}
