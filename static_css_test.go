package main

import (
	"regexp"
	"strings"
	"testing"
)

// A custom property defined in terms of itself is invalid and resolves empty,
// silently dropping every rule that uses it (a bulk find-and-replace once did
// this to seven colour tokens).
func TestStylesheetHasNoSelfReferencingCustomProperties(t *testing.T) {
	raw, err := staticFiles.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	declaration := regexp.MustCompile(`--([\w-]+)\s*:([^;}]*)`)
	for _, match := range declaration.FindAllStringSubmatch(string(raw), -1) {
		name, value := match[1], match[2]
		if strings.Contains(value, "var(--"+name+")") || strings.Contains(value, "var(--"+name+",") {
			t.Errorf("--%s is defined in terms of itself: %s", name, strings.TrimSpace(value))
		}
	}
}
