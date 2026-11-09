package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatsCountsGroupsFiltersShortPlaysAndFindsPeak(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	app := &application{store: s}
	ctx := context.Background()
	sam, _ := s.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "1", DisplayName: "Sam"}, time.Time{})
	jo, _ := s.ensureAccount(ctx, accountInfo{Source: "silo", ExternalID: "7", DisplayName: "Jo"}, time.Time{})
	// 23:30 UTC on Sunday 7 June is 00:30 Monday in London (UTC+1 in summer).
	base := time.Date(2026, 6, 7, 23, 30, 0, 0, time.UTC)
	add := func(key string, account int64, start time.Time, minutes float64, group int64, method string) {
		fields := playFields{}
		fields.set("status", "stopped")
		fields.set("account_id", account)
		fields.set("media_type", "movie")
		fields.set("title", "Big Film")
		fields.set("started_at", start)
		fields.set("ended_at", start.Add(time.Duration(minutes*float64(time.Minute))))
		fields.set("played_s", minutes*60)
		fields.set("play_method", method)
		if group != 0 {
			fields.set("group_id", group)
		}
		if _, _, err := s.upsertPlay(ctx, "plex", key, fields); err != nil {
			t.Fatal(err)
		}
	}
	add("a", sam, base, 30, 1, "direct_play")                     // play 1 starts
	add("b", sam, base.Add(40*time.Minute), 60, 1, "direct_play") // resumed: same group, counts once
	add("c", jo, base.Add(10*time.Minute), 20, 0, "transcode")    // overlaps a: two streams at once
	add("d", jo, base.Add(3*time.Hour), 0.1, 0, "transcode")      // 6 seconds: ignored

	request := httptest.NewRequest("GET", "/api/stats?range=all&tz=Europe/London", nil)
	payload, err := app.buildStats(request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Totals.Plays != 2 || payload.Totals.Users != 2 || payload.Totals.Titles != 1 {
		t.Fatalf("totals = %+v, want 2 plays, 2 users, 1 title", payload.Totals)
	}
	if payload.Methods["direct_play"] != 1 || payload.Methods["transcode"] != 1 {
		t.Fatalf("methods = %v", payload.Methods)
	}
	if payload.Peak.Streams != 2 || payload.Peak.Transcodes != 1 {
		t.Fatalf("peak = %+v, want 2 streams, 1 transcode", payload.Peak)
	}
	// Both plays started after midnight Monday, London time.
	if payload.ByWeekday[0] != 2 || payload.ByHour[0] != 2 {
		t.Fatalf("weekday %v hour %v, want both plays on Monday 00:xx London time", payload.ByWeekday, payload.ByHour)
	}
	if len(payload.TopMovies) != 1 || payload.TopMovies[0].Plays != 2 || payload.TopMovies[0].Users != 2 {
		t.Fatalf("top movies = %+v", payload.TopMovies)
	}
}
