package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// History lists plays by when they ended (or were last seen while active),
// the same time its "ago" label shows, not by when they started.
func TestHistoryOrdersByLastActivity(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	now := time.Now()
	play := func(key, title string, started, ended time.Time) {
		fields := playFields{}
		fields.set("title", title)
		fields.set("started_at", started)
		if ended.IsZero() {
			fields.set("status", "active")
		} else {
			fields.set("status", "stopped")
			fields.set("ended_at", ended)
		}
		if _, _, err := s.upsertPlay(ctx, "plex", key, fields); err != nil {
			t.Fatal(err)
		}
	}
	// E6 started before E5 but finished long after it, as in the report.
	play("e6", "Ride or Die", now.Add(-50*time.Hour), now.Add(-37*time.Minute))
	play("e5", "Speechless", now.Add(-25*time.Hour), now.Add(-24*time.Hour))
	play("e2", "Clean Slate", now.Add(-44*time.Hour), now.Add(-43*time.Hour))
	play("live", "Now Watching", now.Add(-72*time.Hour), time.Time{}) // long-running, still active

	app := &application{store: s}
	recorder := httptest.NewRecorder()
	app.activityJSON(recorder, httptest.NewRequest(http.MethodGet, "/api/activity", nil))
	var entries []struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v (%s)", err, recorder.Body.String())
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Title)
	}
	want := []string{"Now Watching", "Ride or Die", "Speechless", "Clean Slate"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	// The sort uses the index rather than scanning every play.
	var detail string
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM plays p WHERE COALESCE(p.ended_at, p.updated_at) < 1 ORDER BY COALESCE(p.ended_at, p.updated_at) DESC LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		if err := rows.Scan(&id, &parent, &unused, &detail); err == nil {
			plan += detail + "; "
		}
	}
	if !strings.Contains(plan, "plays_last_activity") {
		t.Fatalf("history query does not use plays_last_activity: %s", plan)
	}
}
