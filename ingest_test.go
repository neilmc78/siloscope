package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeServers stands in for Silo and Plex closely enough to drive both
// ingesters through their real HTTP and websocket paths.
type fakeServers struct {
	mu           sync.Mutex
	plexSessions string // XML body for /status/sessions
	siloSessions []map[string]any
	siloHistory  []map[string]any
	plexSockets  chan *websocket.Conn
	siloSockets  chan *websocket.Conn
}

func newFakeServers(t *testing.T) (*fakeServers, *httptest.Server, *httptest.Server) {
	fake := &fakeServers{plexSockets: make(chan *websocket.Conn, 4), siloSockets: make(chan *websocket.Conn, 4)}
	upgrader := websocket.Upgrader{Subprotocols: []string{"silo.events.v2"}}
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		page := func(items any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"has_more": false}})
		}
		// Like Silo, refuse query parameters an operation does not declare.
		allowed := map[string][]string{
			"/api/v2/admin/users": {"limit", "cursor"}, "/api/v2/admin/playback-history": {"limit", "cursor", "ended_after"},
			"/api/v2/admin/sessions": {"limit", "cursor"}, "/api/v2/admin/catalog/items": {"limit", "cursor", "type"},
			"/api/v2/events/ws": {"channels"}, "/api/v1/images/poster-2.png": {"sig"},
		}
		for key := range r.URL.Query() {
			ok := false
			for _, name := range allowed[r.URL.Path] {
				ok = ok || name == key
			}
			if !ok {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
		}
		switch r.URL.Path {
		case "/api/v2/libraries":
			page([]map[string]any{{"id": "1", "name": "Movies", "type": "movies", "paths": []string{"/Media/Movies"}, "enabled": true}})
		case "/api/v2/admin/catalog/items":
			if r.URL.Query().Get("type") == "movie" {
				page([]map[string]any{{"versions": []map[string]any{{"file_id": "f1", "file_size": 1000, "library_id": "1"}, {"file_id": "f2", "file_size": 500, "library_id": "1"}}}})
			} else {
				page([]map[string]any{})
			}
		case "/api/v2/admin/users":
			page([]map[string]any{{"id": "7", "username": "sam", "email": "sam@example.test"}, {"id": "8", "username": "silo-only"}})
		case "/api/v2/admin/users/7/identities":
			page([]map[string]any{{"issuer": "https://plex.tv", "provider_id": "x", "external_subject": "abc123def456"}})
		case "/api/v2/admin/users/8/identities":
			page([]map[string]any{})
		case "/api/v2/admin/playback-history":
			page(fake.siloHistory)
		case "/api/v2/admin/sessions":
			page(fake.siloSessions)
		case "/api/v1/images/poster-2.png":
			if r.Header.Get("Authorization") != "Bearer admin-key" || r.URL.Query().Get("sig") == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
		case "/api/v2/events/ws-ticket":
			if r.Header.Get("Authorization") != "Bearer monitor-key" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"ticket":"t1","protocol":"silo.events.v2"}`))
		case "/api/v2/events/ws":
			if r.URL.Query().Get("channels") != "sessions" || !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "silo.ticket.t1") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fake.mu.Unlock()
			connection, err := upgrader.Upgrade(w, r, nil)
			fake.mu.Lock()
			if err == nil {
				_ = connection.WriteJSON(map[string]any{"type": "hello", "available_channels": []string{"sessions"}})
				fake.siloSockets <- connection
			}
		default:
			http.NotFound(w, r)
		}
	}))
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "plex-token" && r.URL.Query().Get("X-Plex-Token") != "plex-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch r.URL.Path {
		case "/status/sessions":
			_, _ = w.Write([]byte(fake.plexSessions))
		case "/library/metadata/500":
			_, _ = w.Write([]byte(`<MediaContainer><Video ratingKey="500"><Media id="1" bitrate="20000" container="mkv" videoCodec="hevc" videoResolution="4k" width="3840" height="2160" audioCodec="truehd" audioChannels="8"><Part id="9"><Stream streamType="1" codec="hevc" colorTrc="smpte2084" displayTitle="4K HDR10"/><Stream streamType="2" codec="truehd" channels="8" languageCode="eng" selected="1"/></Part></Media></Video></MediaContainer>`))
		case "/accounts":
			_, _ = w.Write([]byte(`<MediaContainer><Account id="0" name=""/><Account id="1" name="Owner Neil"/><Account id="429535384" name="Jo"/></MediaContainer>`))
		case "/status/sessions/history/all":
			if r.Header.Get("X-Plex-Container-Start") == "0" {
				_, _ = w.Write([]byte(`<MediaContainer size="1"><Video historyKey="/status/sessions/history/1" ratingKey="42" type="movie" title="Old Movie" viewedAt="1600000000" accountID="1" deviceID="3"/></MediaContainer>`))
			} else {
				_, _ = w.Write([]byte(`<MediaContainer size="0"/>`))
			}
		case "/:/websockets/notifications":
			fake.mu.Unlock()
			connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			fake.mu.Lock()
			if err == nil {
				fake.plexSockets <- connection
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(silo.Close)
	t.Cleanup(plex.Close)
	return fake, silo, plex
}

func plexSessionXML(state string, offsetMS int) string {
	return fmt.Sprintf(`<MediaContainer size="1"><Video sessionKey="12" ratingKey="500" type="movie" title="Big Film" year="2024" duration="6000000" viewOffset="%d" librarySectionID="1" librarySectionTitle="Movies">
<User id="99" title="Sam P" thumb="https://plex.tv/users/abc123def456/avatar?c=1"/>
<Player address="10.0.0.5" platform="Roku" product="Plex for Roku" state="%s" title="Living Room" version="8.1" local="0" relayed="0" secure="1" machineIdentifier="dev1"/>
<Session id="sess-a" bandwidth="8000" location="wan"/>
<Media bitrate="8000" container="mpegts" videoCodec="h264" videoResolution="1080" audioCodec="aac" audioChannels="2" selected="1"><Part id="9" decision="transcode"><Stream streamType="1" codec="h264" decision="transcode"/><Stream streamType="2" codec="aac" decision="transcode" selected="1"/></Part></Media>
<TranscodeSession videoDecision="transcode" audioDecision="transcode" subtitleDecision="burn" container="mpegts" videoCodec="h264" audioCodec="aac" height="1080" speed="3.1" throttled="1" transcodeHwDecoding="vaapi" transcodeHwEncoding="vaapi"/>
</Video></MediaContainer>`, offsetMS, state)
}

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestIngestersRecordPlaysAndLinkAccounts(t *testing.T) {
	fake, siloServer, plexServer := newFakeServers(t)
	fake.plexSessions = `<MediaContainer size="0"/>`
	fake.siloHistory = []map[string]any{{
		"session_id": "old-1", "user_id": "8", "username": "silo-only", "profile_id": "p1", "profile_name": "Kid",
		"media_item_id": "movie-1", "media_title": "Backfilled", "media_type": "movie",
		"started_at": "2026-01-01T10:00:00Z", "ended_at": "2026-01-01T11:00:00Z", "completed": true,
		"duration_seconds": 3600, "watched_seconds": 3500, "played_seconds": 3400, "paused_seconds": 120,
		"play_method": "direct_play", "video_decision": "direct", "audio_decision": "direct", "client_ip": "192.0.2.4",
	}}

	siloURL, _ := url.Parse(siloServer.URL)
	plexURL, _ := url.Parse(plexServer.URL)
	app := newApplication(config{siloURL: siloURL, apiKey: "admin-key", siloMonitorKey: "monitor-key", plexURL: plexURL, plexToken: "plex-token", timeout: 5 * time.Second})
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	app.store = s
	if app.posters, err = newPosterCache(app, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go newSiloIngester(app, s).run(ctx)
	plex := newPlexIngester(app, s)
	go plex.run(ctx)

	count := func(query string, args ...any) int {
		var n int
		if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Silo backfill and Plex history import.
	waitFor(t, "silo backfill", func() bool {
		return count(`SELECT COUNT(*) FROM plays WHERE source='silo' AND origin='silo_history'`) == 1
	})
	waitFor(t, "plex history import", func() bool { return count(`SELECT COUNT(*) FROM plays WHERE origin='plex_history'`) == 1 })
	var played, paused float64
	var ip string
	if err := s.db.QueryRow(`SELECT played_s, paused_s, ip_address FROM plays WHERE session_key='old-1'`).Scan(&played, &paused, &ip); err != nil || played != 3400 || paused != 120 || ip != "192.0.2.4" {
		t.Fatalf("backfilled silo play = %v %v %q %v", played, paused, ip, err)
	}

	// Silo live: a lifecycle event opens a play, session.stopped finalizes it.
	siloSocket := <-fake.siloSockets
	defer siloSocket.Close()
	row := map[string]any{"session_id": "live-1", "lifecycle_seq": 5, "user_id": "7", "username": "sam", "content_id": "movie-2", "media_title": "Live Film",
		"media_type": "movie", "is_paused": false, "started_at": time.Now().UTC().Format(time.RFC3339), "position_seconds": 10, "file_duration": 7200,
		"play_method": "transcode", "video_decision": "transcode", "audio_decision": "direct", "stream_location": "remote", "client_ip": "198.51.100.2",
		"poster_url": "/api/v1/images/poster-2.png?sig=abc"}
	_ = siloSocket.WriteJSON(map[string]any{"type": "event", "channel": "sessions", "event": "session.started", "data": map[string]any{"session_id": "live-1", "seq": 5, "state": "playing", "session": row}})
	waitFor(t, "silo live play", func() bool {
		return count(`SELECT COUNT(*) FROM plays WHERE session_key='live-1' AND status='active' AND play_method='transcode' AND location='wan'`) == 1
	})
	// Silo's relative, signed poster is cached and served from Siloscope.
	var poster string
	_ = s.db.QueryRow(`SELECT COALESCE(poster_url, '') FROM plays WHERE session_key='live-1'`).Scan(&poster)
	if !strings.HasPrefix(poster, "/api/silo/poster?key=") {
		t.Fatalf("silo poster_url = %q", poster)
	}
	posterResponse := httptest.NewRecorder()
	app.siloPoster(posterResponse, httptest.NewRequest(http.MethodGet, poster, nil))
	if posterResponse.Code != http.StatusOK || posterResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("cached poster = %d %q", posterResponse.Code, posterResponse.Header().Get("Content-Type"))
	}
	fake.mu.Lock()
	fake.siloSessions = []map[string]any{row}
	fake.mu.Unlock()
	sessionsResponse := httptest.NewRecorder()
	app.siloSessionsJSON(sessionsResponse, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if !strings.Contains(sessionsResponse.Body.String(), `"poster_url":"`+strings.ReplaceAll(poster, "&", "\\u0026")) {
		t.Fatalf("playing feed poster not rewritten: %s", sessionsResponse.Body.String())
	}
	fake.mu.Lock()
	fake.siloSessions = nil
	fake.mu.Unlock()

	_ = siloSocket.WriteJSON(map[string]any{"type": "event", "channel": "sessions", "event": "session.stopped", "data": map[string]any{"session_id": "live-1", "seq": 9, "state": "stopped",
		"history": map[string]any{"session_id": "live-1", "lifecycle_seq": 9, "user_id": "7", "username": "sam", "media_item_id": "movie-2", "media_title": "Live Film", "media_type": "movie",
			"started_at": "2026-11-08T10:00:00Z", "ended_at": "2026-11-08T10:30:00Z", "completed": false, "duration_seconds": 7200, "watched_seconds": 1800, "played_seconds": 1750,
			"paused_seconds": 30, "end_reason": "stopped", "play_method": "transcode"}}})
	waitFor(t, "silo stop", func() bool {
		return count(`SELECT COUNT(*) FROM plays WHERE session_key='live-1' AND status='stopped' AND played_s=1750 AND end_reason='stopped'`) == 1
	})
	// A stale event with a lower sequence must not reopen it.
	_ = siloSocket.WriteJSON(map[string]any{"type": "event", "channel": "sessions", "event": "session.paused", "data": map[string]any{"session_id": "live-1", "seq": 6, "state": "paused", "session": row}})
	time.Sleep(200 * time.Millisecond)
	if count(`SELECT COUNT(*) FROM plays WHERE session_key='live-1' AND status='stopped'`) != 1 {
		t.Fatal("stale event reopened a stopped Silo play")
	}

	// Plex live: a notification for an unknown session triggers a read of
	// /status/sessions, later notifications measure watch and pause time.
	plexSocket := <-fake.plexSockets
	defer plexSocket.Close()
	notify := func(state string, offsetMS int) {
		_ = plexSocket.WriteJSON(map[string]any{"NotificationContainer": map[string]any{"type": "playing", "PlaySessionStateNotification": []map[string]any{{"sessionKey": "12", "ratingKey": "500", "viewOffset": offsetMS, "state": state}}}})
	}
	fake.mu.Lock()
	fake.plexSessions = plexSessionXML("playing", 60000)
	fake.mu.Unlock()
	notify("playing", 60000)
	waitFor(t, "plex play", func() bool {
		return count(`SELECT COUNT(*) FROM plays WHERE source='plex' AND session_key='sess-a'`) == 1
	})
	var method, sourceRange, sourceRes, hw, sub, location string
	var bandwidth int
	if err := s.db.QueryRow(`SELECT play_method, source_video_dynamic_range, source_video_resolution, transcode_hw_encode, subtitle_decision, location, bandwidth_kbps FROM plays WHERE session_key='sess-a'`).
		Scan(&method, &sourceRange, &sourceRes, &hw, &sub, &location, &bandwidth); err != nil {
		t.Fatal(err)
	}
	if method != "transcode" || sourceRange != "HDR10" || sourceRes != "4k" || hw != "vaapi" || sub != "burn" || location != "wan" || bandwidth != 8000 {
		t.Fatalf("plex stream details = %s %s %s %s %s %s %d", method, sourceRange, sourceRes, hw, sub, location, bandwidth)
	}

	// Simulate 1 s of playback advancing 1 s, then a pause, then a seek.
	time.Sleep(time.Second)
	notify("playing", 61000)
	time.Sleep(300 * time.Millisecond)
	notify("paused", 61200)
	time.Sleep(time.Second)
	notify("playing", 61200)
	notify("playing", 3000000) // seek: not counted
	waitFor(t, "plex measurement", func() bool {
		return count(`SELECT COUNT(*) FROM plays WHERE session_key='sess-a' AND played_s BETWEEN 1 AND 2 AND paused_s = 1`) == 1
	})
	// The session ends: Plex sends stopped and drops it from /status/sessions.
	fake.mu.Lock()
	fake.plexSessions = `<MediaContainer size="0"/>`
	fake.mu.Unlock()
	notify("stopped", 3000000)
	waitFor(t, "plex stop", func() bool {
		return count(`SELECT COUNT(*) FROM plays WHERE session_key='sess-a' AND status='stopped' AND watched=0`) == 1
	})
	if events := count(`SELECT COUNT(*) FROM play_events e JOIN plays p ON p.id=e.play_id WHERE p.session_key='sess-a' AND e.kind IN ('start','pause','resume','stop')`); events != 4 {
		t.Fatalf("plex play events = %d, want start/pause/resume/stop", events)
	}

	// Linking: Silo "sam" carries the Plex identity abc123def456; Plex user 99's
	// avatar URL carries the same uuid. They end up as one person.
	waitFor(t, "auto link", func() bool {
		_ = s.autoLink(context.Background())
		return count(`SELECT COUNT(DISTINCT person_id) FROM accounts WHERE plex_uuid='abc123def456'`) == 1 &&
			count(`SELECT COUNT(*) FROM accounts WHERE plex_uuid='abc123def456'`) == 2
	})
	if count(`SELECT COUNT(*) FROM accounts a JOIN accounts b ON a.person_id=b.person_id AND a.id<>b.id WHERE a.username='silo-only'`) != 0 {
		t.Fatal("an account without a Plex identity was linked")
	}

	// A manual unlink sticks through later auto-link runs.
	var samID int64
	_ = s.db.QueryRow(`SELECT id FROM accounts WHERE source='silo' AND external_id='7'`).Scan(&samID)
	if err := s.unlinkAccount(context.Background(), samID); err != nil {
		t.Fatal(err)
	}
	_ = s.autoLink(context.Background())
	if count(`SELECT COUNT(DISTINCT person_id) FROM accounts WHERE plex_uuid='abc123def456'`) != 2 {
		t.Fatal("auto link undid a manual unlink")
	}

	// Plex accounts known only by number get their names from the server's account list.
	var joName string
	_ = s.db.QueryRow(`SELECT COALESCE(p.display_name, '') FROM accounts a JOIN people p ON p.id = a.person_id WHERE a.source='plex' AND a.external_id='429535384'`).Scan(&joName)
	if joName != "Jo" {
		t.Fatalf("plex account 429535384 person name = %q, want Jo", joName)
	}

	// Library sizes total the catalog's files per library.
	app.refreshLibraryUsage(context.Background())
	if app.libraryUsage.lastError != "" || len(app.libraryUsage.libraries) != 1 || app.libraryUsage.libraries[0].Bytes != 1500 || app.libraryUsage.libraries[0].Files != 2 {
		t.Fatalf("library usage = %+v, error %q", app.libraryUsage.libraries, app.libraryUsage.lastError)
	}

	// The History API serves all of it, newest first.
	recorder := httptest.NewRecorder()
	app.activityJSON(recorder, httptest.NewRequest(http.MethodGet, "/api/activity", nil))
	var entries []playbackActivity
	if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil || len(entries) != 4 {
		t.Fatalf("activity = %d entries, %v: %s", len(entries), err, recorder.Body.String())
	}
}

func TestResumeGrouping(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account, _ := s.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "1"}, time.Time{})
	base := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	play := func(key string, start time.Time, startPos, endPos float64, watched bool) int64 {
		fields := playFields{}
		fields.set("status", "stopped")
		fields.set("account_id", account)
		fields.set("item_id", "500")
		fields.set("started_at", start)
		fields.set("ended_at", start.Add(30*time.Minute))
		fields.set("start_position_s", startPos)
		fields.set("end_position_s", endPos)
		fields.set("watched", watched)
		id, _, err := s.upsertPlay(ctx, "plex", key, fields)
		if err != nil {
			t.Fatal(err)
		}
		s.finalizeGroup(ctx, id)
		return id
	}
	first := play("a", base, 0, 1800, false)
	second := play("b", base.Add(2*time.Hour), 1790, 3600, false)
	third := play("c", base.Add(3*time.Hour), 600, 900, false) // jumped back: new group
	group := func(id int64) (g int64) {
		_ = s.db.QueryRow(`SELECT group_id FROM plays WHERE id=?`, id).Scan(&g)
		return
	}
	if group(first) != first || group(second) != first || group(third) != third {
		t.Fatalf("groups = %d %d %d", group(first), group(second), group(third))
	}
}

func TestPersonNamesFollowAccountsUntilRenamed(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	name := func(id int64) (value string) {
		_ = s.db.QueryRow(`SELECT p.display_name FROM accounts a JOIN people p ON p.id = a.person_id WHERE a.id = ?`, id).Scan(&value)
		return
	}
	// History import knows only the number; a later account sync supplies the name.
	id, _ := s.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "429535384"}, time.Time{})
	if got := name(id); got != "Plex user 429535384" {
		t.Fatalf("placeholder name = %q", got)
	}
	_, _ = s.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "429535384", DisplayName: "Jo"}, time.Time{})
	_ = s.autoLink(ctx)
	if got := name(id); got != "Jo" {
		t.Fatalf("name after sync = %q, want Jo", got)
	}
	// A manual rename sticks.
	var personID int64
	_ = s.db.QueryRow(`SELECT person_id FROM accounts WHERE id = ?`, id).Scan(&personID)
	_, _ = s.db.Exec(`UPDATE people SET display_name = 'Joanna', name_manual = 1 WHERE id = ?`, personID)
	_, _ = s.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "429535384", DisplayName: "Jo B"}, time.Time{})
	_ = s.autoLink(ctx)
	if got := name(id); got != "Joanna" {
		t.Fatalf("manual name = %q, want Joanna", got)
	}
}
