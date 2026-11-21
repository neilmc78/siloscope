package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Silo builds from late November 2026 send series_title and a subtitle track
// object instead of series_name, episode_name and a display subtitle.
func TestNormalizeSiloSessionsAcrossSiloVersions(t *testing.T) {
	cases := []struct {
		name, in string
		want     map[string]any
	}{
		{
			name: "newer episode gets the older names; subtitle track dropped",
			in:   `{"series_title":"House of the Dragon","media_title":"Rhaenyra the Cruel","season_number":2,"episode_number":8,"subtitle":{"language":"eng","codec":"subrip"}}`,
			want: map[string]any{"series_name": "House of the Dragon", "episode_name": "Rhaenyra the Cruel", "series_title": "House of the Dragon", "media_title": "Rhaenyra the Cruel", "season_number": float64(2), "episode_number": float64(8)},
		},
		{
			name: "newer movie with subtitles off: null subtitle dropped",
			in:   `{"media_title":"Zack Snyder's Justice League","media_type":"movie","subtitle":null}`,
			want: map[string]any{"media_title": "Zack Snyder's Justice League", "media_type": "movie"},
		},
		{
			name: "older episode unchanged",
			in:   `{"series_name":"Harbor Lights","episode_name":"The Lighthouse Keeper","media_title":"The Lighthouse Keeper","season_number":2,"episode_number":4}`,
			want: map[string]any{"series_name": "Harbor Lights", "episode_name": "The Lighthouse Keeper", "media_title": "The Lighthouse Keeper", "season_number": float64(2), "episode_number": float64(4)},
		},
		{
			name: "a display subtitle string is kept",
			in:   `{"media_title":"Clip","subtitle":"Behind the scenes"}`,
			want: map[string]any{"media_title": "Clip", "subtitle": "Behind the scenes"},
		},
	}
	for _, tc := range cases {
		out, err := normalizeSiloSessions([]byte("[" + tc.in + "]"))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got []map[string]any
		if err := json.Unmarshal(out, &got); err != nil || len(got) != 1 {
			t.Fatalf("%s: %s (%v)", tc.name, out, err)
		}
		gotJSON, _ := json.Marshal(got[0])
		wantJSON, _ := json.Marshal(tc.want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, gotJSON, wantJSON)
		}
	}
	if _, err := normalizeSiloSessions([]byte(`{"not":"a list"}`)); err == nil {
		t.Error("a non-list body was accepted")
	}
}

// Live ingest stores a newer Silo episode under its series, as history does.
func TestLiveIngestRecordsSeriesTitleFromNewerSilo(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var row siloSessionRow
	if err := json.Unmarshal([]byte(`{"session_id":"ep-1","user_id":"1","username":"neil","content_id":"ep-500","media_type":"episode",
		"series_title":"House of the Dragon","media_title":"Rhaenyra the Cruel","season_number":2,"episode_number":8,
		"subtitle":{"language":"eng"},"started_at":"2026-11-21T19:40:00Z"}`), &row); err != nil {
		t.Fatal(err)
	}
	ingest := &siloIngester{app: &application{}, store: s}
	if _, _, err := ingest.recordSession(context.Background(), row, time.Now()); err != nil {
		t.Fatal(err)
	}
	var title, series, episode string
	if err := s.db.QueryRow(`SELECT title, series_title, episode_title FROM plays WHERE session_key='ep-1'`).Scan(&title, &series, &episode); err != nil {
		t.Fatal(err)
	}
	if title != "House of the Dragon" || series != "House of the Dragon" || episode != "Rhaenyra the Cruel" {
		t.Fatalf("stored title=%q series=%q episode=%q", title, series, episode)
	}
}
