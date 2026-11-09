package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Silo ingestion.
//
// Silo keeps its own finalized playback log with measured watch time, pause
// time, IP, device and stream decisions. That log is the authority for ended
// plays: Siloscope backfills all of it on first run and then follows it with
// ended_after. Live state comes from the realtime events socket's lifecycle
// events when SILO_MONITOR_API_KEY (an admin:monitor:read key) is set, and from
// a session-list poll when it is not.

type siloIngester struct {
	app       *application
	store     *store
	connected atomic.Bool
	reconcile chan struct{}
	mu        sync.Mutex
	lastError string
}

type siloSessionRow struct {
	SessionID          string   `json:"session_id"`
	LifecycleSeq       *int64   `json:"lifecycle_seq"`
	UserID             string   `json:"user_id"`
	Username           string   `json:"username"`
	ProfileID          string   `json:"profile_id"`
	ProfileName        string   `json:"profile_name"`
	ContentID          string   `json:"content_id"`
	MediaFileID        string   `json:"media_file_id"`
	MediaTitle         string   `json:"media_title"`
	MediaType          string   `json:"media_type"`
	SeriesName         string   `json:"series_name"`
	SeasonNumber       *int     `json:"season_number"`
	EpisodeNumber      *int     `json:"episode_number"`
	EpisodeName        string   `json:"episode_name"`
	PosterURL          string   `json:"poster_url"`
	FileDuration       *float64 `json:"file_duration"`
	Position           *float64 `json:"position_seconds"`
	IsPaused           bool     `json:"is_paused"`
	StartedAt          string   `json:"started_at"`
	ClientName         string   `json:"client_name"`
	ClientVersion      string   `json:"client_version"`
	ClientLabel        string   `json:"client_label"`
	ClientIP           string   `json:"client_ip"`
	DeviceID           string   `json:"device_id"`
	DevicePlatform     string   `json:"device_platform"`
	StreamLocation     string   `json:"stream_location"`
	PlayMethod         string   `json:"play_method"`
	EffectivePlay      string   `json:"effective_play_method"`
	VideoDecision      string   `json:"video_decision"`
	AudioDecision      string   `json:"audio_decision"`
	TranscodeHWAccel   string   `json:"transcode_hw_accel"`
	ToneMapMode        string   `json:"tone_map_mode"`
	SourceContainer    string   `json:"source_container"`
	SourceBitrate      *int64   `json:"source_bitrate_kbps"`
	SourceVideoCodec   string   `json:"source_video_codec"`
	SourceVideoRes     string   `json:"source_video_resolution"`
	SourceVideoHeight  *int     `json:"source_video_height"`
	SourceAudioCodec   string   `json:"source_audio_codec"`
	SourceAudioChannel *int     `json:"source_audio_channels"`
	SourceAudioLang    string   `json:"source_audio_language"`
	OutputContainer    string   `json:"output_container"`
	StreamBitrate      *int64   `json:"stream_bitrate_kbps"`
	TargetBitrate      *int64   `json:"target_bitrate_kbps"`
	TargetVideoCodec   string   `json:"target_video_codec"`
	TargetResolution   string   `json:"target_resolution"`
	TargetAudioCodec   string   `json:"target_audio_codec"`
	TargetAudioChannel *int     `json:"target_audio_channels"`
	NodeName           string   `json:"node_display_name"`
	ExecutionNodeName  string   `json:"routing_execution_node_name"`
}

type siloHistoryEntry struct {
	SessionID      string   `json:"session_id"`
	LifecycleSeq   *int64   `json:"lifecycle_seq"`
	UserID         string   `json:"user_id"`
	Username       string   `json:"username"`
	ProfileID      string   `json:"profile_id"`
	ProfileName    string   `json:"profile_name"`
	MediaItemID    string   `json:"media_item_id"`
	MediaFileID    string   `json:"media_file_id"`
	MediaTitle     string   `json:"media_title"`
	MediaType      string   `json:"media_type"`
	SeriesTitle    string   `json:"series_title"`
	SeasonNumber   *int     `json:"season_number"`
	EpisodeNumber  *int     `json:"episode_number"`
	StartedAt      string   `json:"started_at"`
	EndedAt        string   `json:"ended_at"`
	Completed      bool     `json:"completed"`
	Duration       *float64 `json:"duration_seconds"`
	Watched        float64  `json:"watched_seconds"`
	Played         *float64 `json:"played_seconds"`
	Paused         *float64 `json:"paused_seconds"`
	StartPosition  *float64 `json:"start_position_seconds"`
	PlayMethod     string   `json:"play_method"`
	VideoDecision  *string  `json:"video_decision"`
	AudioDecision  *string  `json:"audio_decision"`
	EndReason      *string  `json:"end_reason"`
	ClientIP       *string  `json:"client_ip"`
	ClientName     *string  `json:"client_name"`
	ClientVersion  *string  `json:"client_version"`
	DeviceID       *string  `json:"device_id"`
	DevicePlatform *string  `json:"device_platform"`
}

func newSiloIngester(app *application, s *store) *siloIngester {
	return &siloIngester{app: app, store: s, reconcile: make(chan struct{}, 1)}
}

func (ingest *siloIngester) request(ctx context.Context, method, path string, query url.Values, key string) ([]byte, int, error) {
	upstream := *ingest.app.config.siloURL
	upstream.Path = strings.TrimRight(upstream.Path, "/") + path
	upstream.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, upstream.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	client := *ingest.app.client
	client.Timeout = 30 * time.Second
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(body) > maxResponseBytes {
		return nil, response.StatusCode, errors.New("Silo response too large")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return body, response.StatusCode, fmt.Errorf("Silo %s returned %s", path, response.Status)
	}
	return body, response.StatusCode, nil
}

// pages walks a v2 {items, page} collection and hands each page's items to visit.
func (ingest *siloIngester) pages(ctx context.Context, path string, query url.Values, visit func(json.RawMessage) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("limit", "200")
	seen := map[string]bool{}
	for page := 0; page < 5000; page++ {
		body, _, err := ingest.request(ctx, http.MethodGet, path, query, ingest.app.config.apiKey)
		if err != nil {
			return err
		}
		var collection struct {
			Items json.RawMessage `json:"items"`
			Page  struct {
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := json.Unmarshal(body, &collection); err != nil {
			return err
		}
		if err := visit(collection.Items); err != nil {
			return err
		}
		if !collection.Page.HasMore || collection.Page.NextCursor == "" || seen[collection.Page.NextCursor] {
			return nil
		}
		seen[collection.Page.NextCursor] = true
		query.Set("cursor", collection.Page.NextCursor)
	}
	return errors.New("Silo collection exceeded the page limit")
}

func (ingest *siloIngester) run(ctx context.Context) {
	ingest.syncAccounts(ctx)
	ingest.syncHistory(ctx)
	ingest.reconcileSessions(ctx)
	if key := ingest.app.config.siloMonitorKey; key != "" {
		go ingest.listen(ctx, key)
	}
	history := time.NewTicker(time.Minute)
	sessions := time.NewTicker(15 * time.Second)
	accounts := time.NewTicker(6 * time.Hour)
	defer history.Stop()
	defer sessions.Stop()
	defer accounts.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-history.C:
			ingest.syncHistory(ctx)
		case <-sessions.C:
			// The socket replaces this poll while it is connected.
			if !ingest.connected.Load() {
				ingest.reconcileSessions(ctx)
			}
		case <-ingest.reconcile:
			ingest.reconcileSessions(ctx)
		case <-accounts.C:
			ingest.syncAccounts(ctx)
		}
	}
}

func (ingest *siloIngester) noteError(err error) {
	if err == nil {
		return
	}
	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	if message := err.Error(); message != ingest.lastError {
		ingest.lastError = message
		log.Printf("silo history: %v", err)
	}
}

// syncAccounts records every Silo account and the plex.tv identity linked to
// it by a Plex sign-in plugin (issuer https://plex.tv, subject = plex.tv uuid).
func (ingest *siloIngester) syncAccounts(ctx context.Context) {
	err := ingest.pages(ctx, "/api/v2/admin/users", nil, func(raw json.RawMessage) error {
		var users []struct {
			ID           string  `json:"id"`
			Username     string  `json:"username"`
			Email        string  `json:"email"`
			LastActiveAt *string `json:"last_active_at"`
		}
		if err := json.Unmarshal(raw, &users); err != nil {
			return err
		}
		for _, user := range users {
			info := accountInfo{Source: "silo", ExternalID: user.ID, Username: user.Username, DisplayName: user.Username, Email: user.Email}
			_ = ingest.pages(ctx, "/api/v2/admin/users/"+url.PathEscape(user.ID)+"/identities", nil, func(raw json.RawMessage) error {
				var identities []struct {
					Issuer          string `json:"issuer"`
					ProviderID      string `json:"provider_id"`
					ProviderName    string `json:"provider_name"`
					ExternalSubject string `json:"external_subject"`
				}
				if json.Unmarshal(raw, &identities) == nil {
					for _, identity := range identities {
						if strings.TrimRight(identity.Issuer, "/") == "https://plex.tv" || strings.Contains(strings.ToLower(identity.ProviderID+identity.ProviderName), "plex") {
							info.PlexUUID = identity.ExternalSubject
						}
					}
				}
				return nil
			})
			var seen time.Time
			if user.LastActiveAt != nil {
				seen, _ = time.Parse(time.RFC3339, *user.LastActiveAt)
			}
			if _, err := ingest.store.ensureAccount(ctx, info, seen); err != nil {
				return err
			}
		}
		return nil
	})
	ingest.noteError(err)
	ingest.noteError(ingest.store.autoLink(ctx))
}

// syncHistory follows Silo's finalized playback log. The first run imports all
// of it; later runs read from a watermark with a small overlap, which the
// idempotent upsert absorbs.
func (ingest *siloIngester) syncHistory(ctx context.Context) {
	query := url.Values{}
	after := ingest.store.getState("silo.history.ended_after")
	if after != "" {
		query.Set("ended_after", after)
	}
	var newest time.Time
	err := ingest.pages(ctx, "/api/v2/admin/playback-history", query, func(raw json.RawMessage) error {
		var entries []siloHistoryEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return err
		}
		for _, entry := range entries {
			origin := "silo_history"
			if after != "" {
				origin = ""
			}
			if err := ingest.recordHistory(ctx, entry, origin); err != nil {
				return err
			}
			if ended, err := time.Parse(time.RFC3339, entry.EndedAt); err == nil && ended.After(newest) {
				newest = ended
			}
		}
		return nil
	})
	if err != nil {
		ingest.noteError(err)
		return
	}
	if !newest.IsZero() {
		_ = ingest.store.setState("silo.history.ended_after", newest.Add(-2*time.Minute).UTC().Format(time.RFC3339))
	} else if after == "" {
		_ = ingest.store.setState("silo.history.ended_after", time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339))
	}
	// Silo guarantees a history row for every ended session within its idle
	// grace plus a sweep. A live row that has gone quiet far longer than that
	// without one is closed so it does not count as active forever.
	_, _ = ingest.store.db.ExecContext(ctx, `UPDATE plays SET status = 'stopped', state = 'stopped', end_reason = 'lost', ended_at = updated_at
		WHERE source = 'silo' AND status = 'active' AND updated_at < ?`, time.Now().Add(-30*time.Minute).UnixMilli())
}

func (ingest *siloIngester) recordHistory(ctx context.Context, entry siloHistoryEntry, origin string) error {
	if entry.SessionID == "" {
		return nil
	}
	fields := playFields{}
	started, _ := time.Parse(time.RFC3339, entry.StartedAt)
	ended, _ := time.Parse(time.RFC3339, entry.EndedAt)
	fields.set("status", "stopped")
	fields.set("state", "stopped")
	fields.set("started_at", started)
	fields.set("ended_at", ended)
	fields.set("origin", origin)
	fields.set("source_seq", entry.LifecycleSeq)
	fields.set("end_reason", entry.EndReason)
	fields.set("profile_id", entry.ProfileID)
	fields.set("profile_name", entry.ProfileName)
	fields.set("item_id", entry.MediaItemID)
	fields.set("media_file_id", entry.MediaFileID)
	fields.set("media_type", entry.MediaType)
	if entry.SeriesTitle != "" || entry.EpisodeNumber != nil {
		fields.set("series_title", entry.SeriesTitle)
		fields.set("episode_title", entry.MediaTitle)
		fields.set("title", firstNonEmpty(entry.SeriesTitle, entry.MediaTitle))
	} else {
		fields.set("title", entry.MediaTitle)
	}
	fields.set("season_number", entry.SeasonNumber)
	fields.set("episode_number", entry.EpisodeNumber)
	fields.set("media_duration_s", entry.Duration)
	fields.set("start_position_s", entry.StartPosition)
	fields.set("end_position_s", entry.Watched)
	fields.set("played_s", entry.Played)
	fields.set("paused_s", entry.Paused)
	fields.set("watched", entry.Completed)
	if entry.Duration != nil && *entry.Duration > 0 {
		fields.set("percent_complete", math.Min(100, entry.Watched / *entry.Duration * 100))
	}
	fields.set("ip_address", entry.ClientIP)
	fields.set("client_name", entry.ClientName)
	fields.set("client_version", entry.ClientVersion)
	fields.set("device_id", entry.DeviceID)
	fields.set("platform", entry.DevicePlatform)
	fields.set("video_decision", entry.VideoDecision)
	fields.set("audio_decision", entry.AudioDecision)
	fields.set("play_method", siloPlayMethod(entry.PlayMethod, deref(entry.VideoDecision), deref(entry.AudioDecision)))
	if entry.UserID != "" {
		accountID, err := ingest.store.ensureAccount(ctx, accountInfo{Source: "silo", ExternalID: entry.UserID, Username: entry.Username, DisplayName: entry.Username}, ended)
		if err != nil {
			return err
		}
		fields.set("account_id", accountID)
	}
	id, applied, err := ingest.store.upsertPlay(ctx, "silo", entry.SessionID, fields)
	if err != nil || !applied {
		return err
	}
	ingest.store.finalizeGroup(ctx, id)
	return nil
}

func (ingest *siloIngester) recordSession(ctx context.Context, row siloSessionRow, now time.Time) (int64, string, error) {
	if row.SessionID == "" {
		return 0, "", nil
	}
	fields := playFields{}
	state := "playing"
	if row.IsPaused {
		state = "paused"
	}
	started, _ := time.Parse(time.RFC3339, row.StartedAt)
	if started.IsZero() {
		started = now
	}
	fields.set("status", "active")
	fields.set("state", state)
	fields.set("started_at", started)
	fields.set("source_seq", row.LifecycleSeq)
	fields.set("profile_id", row.ProfileID)
	fields.set("profile_name", row.ProfileName)
	fields.set("item_id", row.ContentID)
	fields.set("media_file_id", row.MediaFileID)
	fields.set("media_type", row.MediaType)
	if row.SeriesName != "" {
		fields.set("title", row.SeriesName)
		fields.set("series_title", row.SeriesName)
		fields.set("episode_title", firstNonEmpty(row.EpisodeName, row.MediaTitle))
	} else {
		fields.set("title", row.MediaTitle)
	}
	fields.set("season_number", row.SeasonNumber)
	fields.set("episode_number", row.EpisodeNumber)
	fields.set("poster_url", row.PosterURL)
	fields.set("media_duration_s", row.FileDuration)
	fields.set("end_position_s", row.Position)
	if row.FileDuration != nil && row.Position != nil && *row.FileDuration > 0 {
		fields.set("percent_complete", math.Min(100, *row.Position / *row.FileDuration * 100))
	}
	fields.set("client_name", row.ClientName)
	fields.set("client_version", row.ClientVersion)
	fields.set("device_name", row.ClientLabel)
	fields.set("device_id", row.DeviceID)
	fields.set("platform", row.DevicePlatform)
	fields.set("ip_address", row.ClientIP)
	switch row.StreamLocation {
	case "local":
		fields.set("location", "lan")
	case "remote":
		fields.set("location", "wan")
	}
	fields.set("play_method", siloPlayMethod(firstNonEmpty(row.EffectivePlay, row.PlayMethod), row.VideoDecision, row.AudioDecision))
	fields.set("video_decision", row.VideoDecision)
	fields.set("audio_decision", row.AudioDecision)
	fields.set("transcode_hw_encode", row.TranscodeHWAccel)
	fields.set("tone_mapping", row.ToneMapMode)
	fields.set("source_container", row.SourceContainer)
	fields.set("source_bitrate_kbps", row.SourceBitrate)
	fields.set("source_video_codec", row.SourceVideoCodec)
	fields.set("source_video_resolution", row.SourceVideoRes)
	fields.set("source_video_height", row.SourceVideoHeight)
	fields.set("source_audio_codec", row.SourceAudioCodec)
	fields.set("source_audio_channels", row.SourceAudioChannel)
	fields.set("source_audio_language", row.SourceAudioLang)
	fields.set("stream_container", row.OutputContainer)
	fields.set("stream_bitrate_kbps", firstInt64(row.StreamBitrate, row.TargetBitrate))
	fields.set("stream_video_codec", row.TargetVideoCodec)
	fields.set("stream_video_resolution", row.TargetResolution)
	fields.set("stream_audio_codec", row.TargetAudioCodec)
	fields.set("stream_audio_channels", row.TargetAudioChannel)
	fields.set("node", firstNonEmpty(row.ExecutionNodeName, row.NodeName))
	if row.UserID != "" {
		accountID, err := ingest.store.ensureAccount(ctx, accountInfo{Source: "silo", ExternalID: row.UserID, Username: row.Username, DisplayName: row.Username}, now)
		if err != nil {
			return 0, "", err
		}
		fields.set("account_id", accountID)
	}
	id, _, err := ingest.store.upsertPlay(ctx, "silo", row.SessionID, fields)
	return id, state, err
}

// reconcileSessions reads every live session. Rows missing from the list stay
// open until their history entry arrives, as Silo's reconciliation contract asks.
func (ingest *siloIngester) reconcileSessions(ctx context.Context) {
	now := time.Now()
	err := ingest.pages(ctx, "/api/v2/admin/sessions", nil, func(raw json.RawMessage) error {
		var rows []siloSessionRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		for _, row := range rows {
			if _, err := ingest.recordSessionWithEvent(ctx, row, now); err != nil {
				return err
			}
		}
		return nil
	})
	ingest.noteError(err)
}

// recordSessionWithEvent records the session and a play event when its state changed.
func (ingest *siloIngester) recordSessionWithEvent(ctx context.Context, row siloSessionRow, now time.Time) (int64, error) {
	var previous string
	_ = ingest.store.db.QueryRowContext(ctx, `SELECT COALESCE(state, '') FROM plays WHERE source = 'silo' AND session_key = ?`, row.SessionID).Scan(&previous)
	id, state, err := ingest.recordSession(ctx, row, now)
	if err != nil || id == 0 {
		return id, err
	}
	switch {
	case previous == "":
		ingest.store.addPlayEvent(ctx, id, now, "start", row.Position, "")
	case previous == "playing" && state == "paused":
		ingest.store.addPlayEvent(ctx, id, now, "pause", row.Position, "")
	case previous == "paused" && state == "playing":
		ingest.store.addPlayEvent(ctx, id, now, "resume", row.Position, "")
	}
	return id, nil
}

// listen holds the realtime events socket open, reconnecting with backoff.
// Silo closes monitoring connections after four hours by design.
func (ingest *siloIngester) listen(ctx context.Context, key string) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := ingest.listenOnce(ctx, key)
		ingest.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		ingest.noteError(err)
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (ingest *siloIngester) listenOnce(ctx context.Context, key string) error {
	body, status, err := ingest.request(ctx, http.MethodPost, "/api/v2/events/ws-ticket", nil, key)
	if err != nil {
		if status == http.StatusForbidden {
			return errors.New("SILO_MONITOR_API_KEY must be an administrator's key with the admin:monitor:read scope")
		}
		return err
	}
	var ticket struct {
		Ticket   string `json:"ticket"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(body, &ticket); err != nil || ticket.Ticket == "" {
		return errors.New("Silo returned an invalid events ticket")
	}
	endpoint := *ingest.app.config.siloURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v2/events/ws"
	endpoint.RawQuery = url.Values{"channels": {"sessions"}}.Encode()
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	dialer := websocket.Dialer{
		HandshakeTimeout:  10 * time.Second,
		Proxy:             http.ProxyFromEnvironment,
		Subprotocols:      []string{"silo.events.v2", "silo.ticket." + ticket.Ticket},
		EnableCompression: true,
	}
	connection, response, err := dialer.DialContext(ctx, endpoint.String(), nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("events socket: %w", err)
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	connection.SetReadLimit(maxResponseBytes)
	connection.SetPingHandler(func(data string) error {
		connection.SetReadDeadline(time.Now().Add(90 * time.Second))
		return connection.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	for {
		connection.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := connection.ReadMessage()
		if err != nil {
			return err
		}
		var frame struct {
			Type    string          `json:"type"`
			Channel string          `json:"channel"`
			Event   string          `json:"event"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "hello", "subscribed":
			if frame.Type == "subscribed" || !ingest.connected.Load() {
				ingest.connected.Store(true)
				ingest.requestReconcile()
			}
		case "snapshot":
			ingest.requestReconcile()
		case "access_changed":
			return errors.New("events socket access changed; reconnecting")
		case "event":
			if frame.Channel == "sessions" {
				ingest.handleEvent(ctx, frame.Event, frame.Data)
			}
		}
	}
}

func (ingest *siloIngester) requestReconcile() {
	select {
	case ingest.reconcile <- struct{}{}:
	default:
	}
}

func (ingest *siloIngester) handleEvent(ctx context.Context, event string, raw json.RawMessage) {
	now := time.Now()
	switch event {
	case "sessions.replaced":
		ingest.requestReconcile()
	case "session.started", "session.paused", "session.resumed":
		var data struct {
			SessionID string          `json:"session_id"`
			Seq       int64           `json:"seq"`
			Session   *siloSessionRow `json:"session"`
		}
		if json.Unmarshal(raw, &data) != nil || data.Session == nil {
			ingest.requestReconcile()
			return
		}
		if data.Session.LifecycleSeq == nil {
			data.Session.LifecycleSeq = &data.Seq
		}
		_, err := ingest.recordSessionWithEvent(ctx, *data.Session, now)
		ingest.noteError(err)
	case "session.stopped":
		var data struct {
			SessionID string            `json:"session_id"`
			Seq       int64             `json:"seq"`
			History   *siloHistoryEntry `json:"history"`
		}
		if json.Unmarshal(raw, &data) != nil {
			return
		}
		if data.History != nil {
			if data.History.LifecycleSeq == nil {
				data.History.LifecycleSeq = &data.Seq
			}
			ingest.noteError(ingest.recordHistory(ctx, *data.History, ""))
		}
		var id int64
		_ = ingest.store.db.QueryRowContext(ctx, `SELECT id FROM plays WHERE source = 'silo' AND session_key = ?`, data.SessionID).Scan(&id)
		ingest.store.addPlayEvent(ctx, id, now, "stop", nil, "")
	}
}

// siloPlayMethod maps Silo's method and decisions to direct_play, direct_stream
// or transcode. Audio-only transcodes count as transcode; video_decision and
// audio_decision keep the detail.
func siloPlayMethod(method, video, audio string) string {
	if strings.EqualFold(video, "transcode") || strings.EqualFold(audio, "transcode") {
		return "transcode"
	}
	switch strings.ToLower(strings.ReplaceAll(method, "_", "")) {
	case "transcode":
		return "transcode"
	case "remux", "directstream", "copy":
		return "direct_stream"
	case "directplay", "direct":
		if strings.EqualFold(video, "remux") || strings.EqualFold(audio, "remux") {
			return "direct_stream"
		}
		return "direct_play"
	}
	if strings.EqualFold(video, "remux") || strings.EqualFold(audio, "remux") {
		return "direct_stream"
	}
	if video != "" || audio != "" {
		return "direct_play"
	}
	return ""
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstInt64(values ...*int64) *int64 {
	for _, value := range values {
		if value != nil && *value > 0 {
			return value
		}
	}
	return nil
}
