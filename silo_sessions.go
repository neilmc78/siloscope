package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// normalizeSiloSessions keeps Silo's live sessions in the shape the Playing
// tab and push alerts read, across Silo versions.
//
// Newer Silo builds dropped series_name and episode_name from
// /api/v2/admin/sessions: an episode carries series_title, and media_title
// is the episode's own title. subtitle also changed from a display string to
// the subtitle track being delivered (an object, or null). Older builds are
// passed through unchanged.
func normalizeSiloSessions(body []byte) ([]byte, error) {
	var sessions []map[string]json.RawMessage
	if err := json.Unmarshal(body, &sessions); err != nil {
		return nil, fmt.Errorf("decode Silo sessions: %w", err)
	}
	for _, session := range sessions {
		normalizeSiloSession(session)
	}
	return json.Marshal(sessions)
}

func normalizeSiloSession(session map[string]json.RawMessage) {
	if jsonString(session["series_name"]) == "" {
		if series := jsonString(session["series_title"]); series != "" {
			session["series_name"] = session["series_title"]
			if jsonString(session["episode_name"]) == "" && session["media_title"] != nil {
				session["episode_name"] = session["media_title"]
			}
		}
	}
	// Only a string is a display subtitle; the track object is not.
	if raw, ok := session["subtitle"]; ok && !isJSONString(raw) {
		delete(session, "subtitle")
	}
}

func isJSONString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

func jsonString(raw json.RawMessage) string {
	var value string
	if !isJSONString(raw) || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}
