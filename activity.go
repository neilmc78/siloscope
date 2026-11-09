package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runActivity polls live sessions for push notifications and service alerts.
// Playback history no longer depends on it: the Silo and Plex ingesters record
// plays from events. Without push notifications nothing here needs to run.
func (app *application) runActivity(ctx context.Context) {
	if app.notifications == nil {
		return
	}
	app.pollActivity(ctx)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			app.pollActivity(ctx)
		}
	}
}

func (app *application) pollActivity(ctx context.Context) {
	var tasks sync.WaitGroup
	tasks.Add(1)
	go func() { defer tasks.Done(); app.pollNodeAlerts(ctx) }()
	for _, source := range []string{"Silo", "Plex"} {
		if source == "Plex" && app.config.plexURL == nil {
			continue
		}
		tasks.Add(1)
		go func(source string) {
			defer tasks.Done()
			var sessions []notificationSession
			var available bool
			if source == "Silo" {
				body, _, err := app.fetch(ctx, "sessions", "/api/v2/admin/sessions", app.config.cacheTTL)
				available = err == nil && json.Unmarshal(body, &sessions) == nil && sessions != nil
			} else {
				body, err := app.plexSessionData(ctx)
				var payload struct {
					Sessions []notificationSession `json:"sessions"`
				}
				available = err == nil && json.Unmarshal(body, &payload) == nil && payload.Sessions != nil
				sessions = payload.Sessions
			}
			now := time.Now()
			if app.notifications != nil {
				app.notifications.observeService(source, source, &available, now)
			}
			if available && app.notifications != nil {
				app.notifications.observeSessions(source, sessions, now)
			}
		}(source)
	}
	tasks.Wait()
}

// playbackActivity is one History row. The first block keeps the fields the
// History tab already reads; the rest expose what the play store now records.
type playbackActivity struct {
	Poster          string   `json:"poster_url,omitempty"`
	Series          string   `json:"series_name,omitempty"`
	Season          *int64   `json:"season_number,omitempty"`
	Episode         *int64   `json:"episode_number,omitempty"`
	EpisodeName     string   `json:"episode_name,omitempty"`
	ID              string   `json:"id"`
	Source          string   `json:"source"`
	Title           string   `json:"title"`
	User            string   `json:"user"`
	Method          string   `json:"method"`
	Status          string   `json:"status"`
	StartedAt       string   `json:"started_at"`
	LastSeen        string   `json:"last_seen"`
	EndedAt         *string  `json:"ended_at,omitempty"`
	AlreadyActive   bool     `json:"already_active"`
	DurationSeconds int64    `json:"duration_seconds"`
	PersonID        *int64   `json:"person_id,omitempty"`
	Person          string   `json:"person,omitempty"`
	Profile         string   `json:"profile,omitempty"`
	State           string   `json:"state,omitempty"`
	Origin          string   `json:"origin"`
	EndReason       string   `json:"end_reason,omitempty"`
	PlayedSeconds   *float64 `json:"played_seconds,omitempty"`
	PausedSeconds   *float64 `json:"paused_seconds,omitempty"`
	Percent         *float64 `json:"percent_complete,omitempty"`
	Watched         *bool    `json:"watched,omitempty"`
	Client          string   `json:"client,omitempty"`
	Platform        string   `json:"platform,omitempty"`
	IP              string   `json:"ip_address,omitempty"`
	Location        string   `json:"location,omitempty"`
	Quality         string   `json:"quality,omitempty"`
}

var activityMethods = map[string]string{"direct_play": "Direct play", "direct_stream": "Direct stream", "transcode": "Transcoding"}

// activityJSON serves GET /api/activity?limit=N&before=<unix ms>.
func (app *application) activityJSON(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	if app.store == nil {
		_, _ = writer.Write([]byte("[]"))
		return
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	before, _ := strconv.ParseInt(request.URL.Query().Get("before"), 10, 64)
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := app.store.db.QueryContext(request.Context(), `SELECT p.id, p.source, COALESCE(p.title, ''), COALESCE(p.series_title, ''),
			p.season_number, p.episode_number, COALESCE(p.episode_title, ''), COALESCE(p.poster_url, ''),
			COALESCE(NULLIF(a.display_name, ''), NULLIF(a.username, ''), pe.display_name, ''), pe.id, COALESCE(pe.display_name, ''), COALESCE(p.profile_name, ''),
			COALESCE(p.play_method, ''), p.status, COALESCE(p.state, ''), p.origin, COALESCE(p.end_reason, ''),
			p.started_at, p.ended_at, p.updated_at, p.played_s, p.paused_s, p.percent_complete, p.watched,
			COALESCE(p.client_name, p.client_product, ''), COALESCE(p.platform, ''), COALESCE(p.ip_address, ''), COALESCE(p.location, ''),
			COALESCE(p.source_video_resolution, '')
		FROM plays p
		LEFT JOIN accounts a ON a.id = p.account_id
		LEFT JOIN people pe ON pe.id = a.person_id
		WHERE p.started_at < ?
		ORDER BY p.started_at DESC LIMIT ?`, before, limit)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "History could not be read")
		return
	}
	defer rows.Close()
	entries := []playbackActivity{}
	for rows.Next() {
		var entry playbackActivity
		var id, started, updated int64
		var ended, season, episode, personID sql.NullInt64
		var played, paused, percent sql.NullFloat64
		var watched sql.NullBool
		var source, method, series, episodeTitle string
		if err := rows.Scan(&id, &source, &entry.Title, &series, &season, &episode, &episodeTitle, &entry.Poster,
			&entry.User, &personID, &entry.Person, &entry.Profile, &method, &entry.Status, &entry.State, &entry.Origin, &entry.EndReason,
			&started, &ended, &updated, &played, &paused, &percent, &watched,
			&entry.Client, &entry.Platform, &entry.IP, &entry.Location, &entry.Quality); err != nil {
			writeJSONError(writer, http.StatusInternalServerError, "History could not be read")
			return
		}
		entry.ID = strconv.FormatInt(id, 10)
		entry.Source = map[string]string{"silo": "Silo", "plex": "Plex"}[source]
		if series != "" {
			entry.Series = series
			entry.EpisodeName = episodeTitle
			entry.Title = series + ": " + episodeTitle
		}
		if season.Valid {
			entry.Season = &season.Int64
		}
		if episode.Valid {
			entry.Episode = &episode.Int64
		}
		if entry.Profile != "" && entry.Profile != entry.User {
			entry.User = entry.User + " (" + entry.Profile + ")"
		}
		if entry.User == "" {
			entry.User = entry.Person
		}
		if personID.Valid {
			entry.PersonID = &personID.Int64
		}
		entry.Method = activityMethods[method]
		entry.StartedAt = time.UnixMilli(started).UTC().Format(time.RFC3339Nano)
		entry.LastSeen = time.UnixMilli(updated).UTC().Format(time.RFC3339Nano)
		end := updated
		if ended.Valid {
			end = ended.Int64
			text := time.UnixMilli(ended.Int64).UTC().Format(time.RFC3339Nano)
			entry.EndedAt = &text
			entry.LastSeen = text
		}
		if played.Valid {
			entry.PlayedSeconds = &played.Float64
			entry.DurationSeconds = int64(played.Float64)
		} else {
			entry.DurationSeconds = max(0, (end-started)/1000)
		}
		if paused.Valid {
			entry.PausedSeconds = &paused.Float64
		}
		if percent.Valid {
			entry.Percent = &percent.Float64
		}
		if watched.Valid {
			entry.Watched = &watched.Bool
		}
		entries = append(entries, entry)
	}
	_ = json.NewEncoder(writer).Encode(entries)
}

func activityText(value string) string {
	text := []rune(strings.TrimSpace(value))
	if len(text) > 180 {
		text = text[:180]
	}
	return string(text)
}
