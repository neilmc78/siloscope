package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	if payload.ByWeekday[0] != 2 || payload.ByHour[0] != 2 || payload.ByWeekdayHour[0][0] != 2 {
		t.Fatalf("weekday %v hour %v, want both plays on Monday 00:xx London time", payload.ByWeekday, payload.ByHour)
	}
	if len(payload.TopMovies) != 1 || payload.TopMovies[0].Plays != 2 || payload.TopMovies[0].Users != 2 {
		t.Fatalf("top movies = %+v", payload.TopMovies)
	}
}

func TestAutoLinkMatchesEmailAcrossServers(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	add := func(source, id, email, uuid string) int64 {
		account, err := s.ensureAccount(ctx, accountInfo{Source: source, ExternalID: id, Username: source + id, Email: email, PlexUUID: uuid}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	person := func(id int64) (value int64) {
		_ = s.db.QueryRow(`SELECT person_id FROM accounts WHERE id = ?`, id).Scan(&value)
		return
	}
	siloOwner := add("silo", "1", "Owner@Example.com", "")           // password sign-in, no Plex identity
	plexOwner := add("plex", "1", "owner@example.com", "uuid-owner") // same email, different case
	siloClash := add("silo", "2", "shared@example.com", "uuid-a")    // same email, different plex.tv IDs
	plexClash := add("plex", "2", "shared@example.com", "uuid-b")
	placeholder := add("silo", "3", "sam@plugin-4.invalid", "")
	plexSam := add("plex", "3", "sam@plugin-4.invalid", "")
	siloA := add("silo", "4", "twin@example.com", "") // two Silo accounts: same server, never merged by email
	siloB := add("silo", "5", "twin@example.com", "")
	if err := s.autoLink(ctx); err != nil {
		t.Fatal(err)
	}
	if person(siloOwner) != person(plexOwner) {
		t.Fatal("Silo and Plex accounts with the same email were not linked")
	}
	if person(siloClash) == person(plexClash) {
		t.Fatal("accounts with different plex.tv IDs were linked by email")
	}
	if person(placeholder) == person(plexSam) {
		t.Fatal("a placeholder .invalid email was used to link")
	}
	if person(siloA) == person(siloB) {
		t.Fatal("two accounts on the same server were linked by email")
	}
	// A manual unlink survives later automatic matching.
	if err := s.unlinkAccount(ctx, siloOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.autoLink(ctx); err != nil {
		t.Fatal(err)
	}
	if person(siloOwner) == person(plexOwner) {
		t.Fatal("auto link undid a manual unlink")
	}
}

// Stream quality counts every stream: reasons, hardware against software,
// resolution changes and per-device direct play, plus bandwidth from the
// per-minute samples (the busiest minute and the average while streaming).
func TestStreamQuality(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	app := &application{store: s}
	ctx := context.Background()
	account, _ := s.ensureAccount(ctx, accountInfo{Source: "silo", ExternalID: "1", DisplayName: "Neil"}, time.Time{})
	base := time.Date(2026, 6, 8, 20, 0, 0, 0, time.UTC)
	add := func(key, client, method, reasons, hw, from, to string) {
		fields := playFields{}
		fields.set("status", "stopped")
		fields.set("account_id", account)
		fields.set("media_type", "movie")
		fields.set("title", "Film "+key)
		fields.set("started_at", base)
		fields.set("ended_at", base.Add(time.Hour))
		fields.set("played_s", 3600.0)
		fields.set("client_name", client)
		fields.set("play_method", method)
		fields.set("transcode_reason", reasons)
		fields.set("transcode_hw_encode", hw)
		fields.set("source_video_resolution", from)
		fields.set("stream_video_resolution", to)
		if _, _, err := s.upsertPlay(ctx, "silo", key, fields); err != nil {
			t.Fatal(err)
		}
	}
	add("a", "Silo Web", "transcode", "video_codec_unsupported,audio_channels_exceed", "vaapi", "4k", "1080p")
	add("b", "Silo Web", "transcode", "video_codec_unsupported", "none", "1080p", "1080p")
	add("c", "Silo Web", "direct_play", "", "", "1080p", "")
	add("d", "Apple TV", "direct_play", "", "", "4k", "")
	add("e", "Plex for Android", "transcode", "video_transcode,subtitle_burn_in", "", "2160", "720")

	minute := func(m int) int64 { return base.Add(time.Duration(m) * time.Minute).UnixMilli() }
	for _, sample := range [][5]int64{
		{minute(0), 8000, 8000, 0, 1},
		{minute(1), 20000, 5000, 15000, 2},
		{minute(2), 0, 0, 0, 0}, // nothing playing: not part of the average
	} {
		if _, err := s.db.Exec(`INSERT INTO stream_samples (at, source, streams, direct_plays, direct_streams, transcodes, bandwidth_kbps, lan_kbps, wan_kbps)
			VALUES (?, 'silo', ?, 0, 0, 0, ?, ?, ?)`, sample[0], sample[4], sample[1], sample[2], sample[3]); err != nil {
			t.Fatal(err)
		}
	}

	payload, err := app.buildStats(httptest.NewRequest("GET", "/api/stats?range=all", nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	q := payload.Quality
	if len(q.Reasons) != 4 || q.Reasons[0] != (rankedName{Name: "Video codec unsupported", Plays: 2}) {
		t.Fatalf("reasons = %+v", q.Reasons)
	}
	labels := map[string]bool{}
	for _, reason := range q.Reasons {
		labels[reason.Name] = true
	}
	for _, want := range []string{"Audio channels exceed", "Video transcode", "Subtitle burn in"} {
		if !labels[want] {
			t.Errorf("reason %q missing from %+v", want, q.Reasons)
		}
	}
	if q.HardwareTranscode != 1 || q.SoftwareTranscode != 2 {
		t.Fatalf("hardware %d software %d, want 1 and 2", q.HardwareTranscode, q.SoftwareTranscode)
	}
	// 1080p → 1080p is no change; 2160 and 4k read the same.
	if len(q.Resolutions) != 2 || q.Resolutions[0].Name != "4K → 1080p" || q.Resolutions[1].Name != "4K → 720p" {
		t.Fatalf("resolutions = %+v", q.Resolutions)
	}
	if len(q.Devices) != 3 || q.Devices[0] != (deviceQuality{Name: "Silo Web", Plays: 3, DirectPlay: 1, Transcode: 2}) {
		t.Fatalf("devices = %+v", q.Devices)
	}
	if q.PeakBandwidth == nil || q.PeakBandwidth.Kbps != 20000 || !q.PeakBandwidth.At.Equal(base.Add(time.Minute)) {
		t.Fatalf("peak bandwidth = %+v", q.PeakBandwidth)
	}
	var day *bandwidthBucket
	for i := range q.Bandwidth {
		if q.Bandwidth[i].PeakKbps > 0 {
			day = &q.Bandwidth[i]
		}
	}
	if day == nil || day.PeakKbps != 20000 || day.LANKbps != 6500 || day.WANKbps != 7500 {
		t.Fatalf("bandwidth bucket = %+v, want peak 20000, LAN 6500, WAN 7500 (averaged over the 2 busy minutes)", day)
	}

	// A person's page has quality but no server-wide bandwidth.
	person, err := app.buildStats(httptest.NewRequest("GET", "/api/stats?range=all", nil), 1)
	if err == nil && (person.Quality.Bandwidth != nil || person.Quality.PeakBandwidth != nil) {
		t.Fatalf("person bandwidth = %+v", person.Quality)
	}
}

func TestReasonLabels(t *testing.T) {
	for code, want := range map[string]string{
		"video_codec_unsupported": "Video codec unsupported",
		"hdr_tone_mapping":        "HDR tone mapping",
		" audio_transcode ":       "Audio transcode",
		"bitrate_exceeds_limit":   "Bitrate exceeds limit",
		"":                        "",
	} {
		if got := reasonLabel(code); got != want {
			t.Errorf("reasonLabel(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestTitleStats(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	app := &application{store: s}
	ctx := context.Background()
	sam, _ := s.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "1", DisplayName: "Sam"}, time.Time{})
	jo, _ := s.ensureAccount(ctx, accountInfo{Source: "silo", ExternalID: "7", DisplayName: "Jo"}, time.Time{})
	base := time.Now().Add(-48 * time.Hour)
	key := 0
	add := func(source string, account int64, series string, season, episode int, hours float64, watched bool, group int64) {
		key++
		fields := playFields{}
		fields.set("status", "stopped")
		fields.set("account_id", account)
		fields.set("media_type", "episode")
		fields.set("title", "Episode")
		fields.set("series_title", series)
		fields.set("season_number", season)
		fields.set("episode_number", episode)
		start := base.Add(time.Duration(hours * float64(time.Hour)))
		fields.set("started_at", start)
		fields.set("ended_at", start.Add(40*time.Minute))
		fields.set("played_s", 2400)
		fields.set("watched", watched)
		fields.set("play_method", "direct_play")
		if group != 0 {
			fields.set("group_id", group)
		}
		if _, _, err := s.upsertPlay(ctx, source, "k"+string(rune('a'+key)), fields); err != nil {
			t.Fatal(err)
		}
	}
	add("plex", sam, "The Show", 1, 1, 0, true, 0)
	add("plex", sam, "The Show", 1, 2, 1, false, 900) // stopped part way...
	add("plex", sam, "The Show", 1, 2, 2, true, 900)  // ...and resumed: one viewing
	add("plex", sam, "The Show", 1, 1, 3, true, 0)    // a rewatch
	add("silo", jo, "the show", 1, 1, 4, true, 0)     // the same show on the other server
	add("silo", jo, "Another Show", 1, 1, 5, true, 0)

	recorder := httptest.NewRecorder()
	app.titleStatsJSON(recorder, httptest.NewRequest("GET", "/api/stats/title?key=show:the+show&range=7d", nil))
	if recorder.Code != 200 {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	var title titleStats
	if err := json.Unmarshal(recorder.Body.Bytes(), &title); err != nil {
		t.Fatal(err)
	}
	if title.Kind != "show" || title.Totals.Plays != 4 || title.Totals.Viewers != 2 || title.Totals.Completions != 4 || title.Totals.Rewatches != 1 {
		t.Fatalf("title = %+v, want a show with 4 plays, 2 viewers, 4 completions, 1 rewatch", title.Totals)
	}
	if len(title.Viewers) != 2 || title.Viewers[0].Name != "Jo" || title.Viewers[1].Completed != 2 || title.Viewers[1].Rewatches != 1 || title.Viewers[1].LastEpisode != "S1 E1" {
		t.Fatalf("viewers = %+v, want Jo (latest) then Sam with 2 episodes, 1 rewatch, last S1 E1", title.Viewers)
	}
	if len(title.Episodes) != 2 || title.Episodes[0].Label != "S1 E1" || title.Episodes[0].Plays != 3 || title.Episodes[0].Viewers != 2 {
		t.Fatalf("episodes = %+v, want S1 E1 first with 3 plays by 2 viewers", title.Episodes)
	}

	recorder = httptest.NewRecorder()
	app.titleStatsJSON(recorder, httptest.NewRequest("GET", "/api/stats/title?key=movie:nothing&range=7d", nil))
	if recorder.Code != 404 {
		t.Fatalf("unknown title: status %d, want 404", recorder.Code)
	}
}

func TestLibraryStats(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	app := &application{store: s}
	ctx := context.Background()
	sam, _ := s.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "1", DisplayName: "Sam"}, time.Time{})
	now := time.Now()
	daysAgo := func(days int) int64 { return now.AddDate(0, 0, -days).UnixMilli() }
	episode := func(id string, number int64, added int64) catalogItem {
		return catalogItem{ContentID: id, Type: "episode", Title: "Ep", SeriesID: "series-1", SeriesTitle: "The Show",
			SeasonNumber: sql.NullInt64{Int64: 1, Valid: true}, EpisodeNumber: sql.NullInt64{Int64: number, Valid: true},
			AddedAt: added, LibraryIDs: []string{"2"}, Bytes: 1 << 30}
	}
	if err := s.replaceCatalog(ctx, []catalogItem{
		{ContentID: "movie-a", Type: "movie", Title: "Arrival", AddedAt: daysAgo(2), LibraryIDs: []string{"1"}, Bytes: 10 << 30},
		{ContentID: "movie-b", Type: "movie", Title: "Big Unwatched", AddedAt: daysAgo(60), LibraryIDs: []string{"1"}, Bytes: 20 << 30},
		episode("ep-0", 1, daysAgo(40)),
		episode("ep-1", 2, daysAgo(1)),
		episode("ep-2", 3, daysAgo(1)+1000),
	}); err != nil {
		t.Fatal(err)
	}
	play := func(key, mediaType, title, series string) {
		fields := playFields{}
		fields.set("status", "stopped")
		fields.set("account_id", sam)
		fields.set("media_type", mediaType)
		fields.set("title", title)
		if series != "" {
			fields.set("series_title", series)
			fields.set("episode_number", 2)
		}
		fields.set("started_at", now.Add(-time.Hour))
		fields.set("ended_at", now)
		fields.set("played_s", 3000)
		if _, _, err := s.upsertPlay(ctx, "plex", key, fields); err != nil {
			t.Fatal(err)
		}
	}
	play("a", "movie", "ARRIVAL", "") // Plex play of a Silo title, different case
	play("b", "episode", "Ep", "The Show")

	items, err := s.loadCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/stats/libraries?range=7d", nil)
	r := parseStatsRange(request)
	plays, _ := app.loadStatsPlays(request, r, "", 0)
	played, _ := s.playedTitleKeys(ctx)
	stats := buildLibraryStats([]libraryUsage{{ID: "1", Name: "Movies", Type: "movies"}, {ID: "2", Name: "TV", Type: "tv"}},
		items, plays, played, map[string]string{"series-1": "/poster"}, r)

	movies, tv := stats.Libraries[0], stats.Libraries[1]
	if movies.Titles != 2 || movies.AddedTitles != 1 || movies.Plays != 1 || movies.Viewers != 1 || movies.NeverPlayed != 1 {
		t.Fatalf("movies = %+v, want 2 titles, 1 added, 1 play by 1 viewer, 1 never played", movies)
	}
	if tv.Titles != 1 || tv.Episodes != 3 || tv.AddedTitles != 1 || tv.AddedEpisodes != 2 || tv.Plays != 1 || tv.NeverPlayed != 0 {
		t.Fatalf("tv = %+v, want 1 show, 3 episodes, 2 added, 1 play", tv)
	}
	if len(stats.Recent) != 3 || stats.Recent[0].Title != "The Show" || stats.Recent[0].Subtitle != "2 new episodes" ||
		stats.Recent[0].Poster != "/poster" || stats.Recent[1].Title != "Arrival" || stats.Recent[0].Library != "TV" {
		t.Fatalf("recently added = %+v, want The Show (2 new episodes, poster, TV) then Arrival", stats.Recent)
	}
	if len(stats.NeverPlayed) != 1 || stats.NeverPlayed[0].Title != "Big Unwatched" {
		t.Fatalf("never played = %+v, want Big Unwatched", stats.NeverPlayed)
	}
	total := 0
	for _, bucket := range stats.AddedOverTime {
		for _, count := range bucket.Items {
			total += count
		}
	}
	if total != 3 {
		t.Fatalf("added over time totals %d, want 3 items added in the week", total)
	}
}

func TestSiloHistoryKeepsTranscodeReasons(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ingest := &siloIngester{store: s}
	ctx := context.Background()
	var entry siloHistoryEntry
	if err := json.Unmarshal([]byte(`{"session_id":"s1","user_id":"7","username":"jo","media_title":"Arrival","media_type":"movie",
		"started_at":"2026-11-25T20:00:00Z","ended_at":"2026-11-25T22:00:00Z","play_method":"transcode",
		"adaptation_reasons":["video_codec_unsupported","audio_channels_exceed"]}`), &entry); err != nil {
		t.Fatal(err)
	}
	if err := ingest.recordHistory(ctx, entry, "silo_history"); err != nil {
		t.Fatal(err)
	}
	var reasons string
	if err := s.db.QueryRow(`SELECT COALESCE(transcode_reason, '') FROM plays WHERE session_key = 's1'`).Scan(&reasons); err != nil {
		t.Fatal(err)
	}
	if reasons != "video_codec_unsupported,audio_channels_exceed" {
		t.Fatalf("transcode_reason = %q, want both reasons from the history row", reasons)
	}
}
