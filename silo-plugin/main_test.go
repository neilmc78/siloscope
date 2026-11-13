package main

import (
	"context"
	"strings"
	"testing"

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
}
