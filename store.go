package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// The play store is the single source of truth for playback history. Silo and
// Plex ingesters write to it; the History tab and (later) the stats views read
// from it. Columns cover every per-stream detail either server reports so that
// later analytics work only adds queries, not schema changes.

// migrations are applied in order and recorded in schema_migrations. Never
// edit a released migration; append a new one.
var migrations = []string{
	// 1: initial schema.
	`
CREATE TABLE people (
	id           INTEGER PRIMARY KEY,
	display_name TEXT NOT NULL,
	notes        TEXT,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);

-- One row per login on a media server. Every account belongs to a person;
-- linking two accounts moves them under one person.
CREATE TABLE accounts (
	id           INTEGER PRIMARY KEY,
	source       TEXT NOT NULL CHECK (source IN ('silo', 'plex')),
	external_id  TEXT NOT NULL,              -- Silo user_id, Plex account id ("1" is the server owner)
	username     TEXT,
	display_name TEXT,
	email        TEXT,
	plex_uuid    TEXT,                       -- plex.tv uuid; on Silo, the Plex sign-in identity's subject
	thumb_url    TEXT,
	person_id    INTEGER NOT NULL REFERENCES people(id),
	link_method  TEXT NOT NULL DEFAULT 'auto' CHECK (link_method IN ('auto', 'manual')),
	first_seen   INTEGER,
	last_seen    INTEGER,
	updated_at   INTEGER NOT NULL,
	extra        TEXT,
	UNIQUE (source, external_id)
);
CREATE INDEX accounts_person ON accounts(person_id);
CREATE INDEX accounts_plex_uuid ON accounts(plex_uuid) WHERE plex_uuid IS NOT NULL;

-- One row per playback attempt. Resumed attempts share group_id.
CREATE TABLE plays (
	id                        INTEGER PRIMARY KEY,
	source                    TEXT NOT NULL CHECK (source IN ('silo', 'plex')),
	session_key               TEXT NOT NULL,
	origin                    TEXT NOT NULL DEFAULT 'live' CHECK (origin IN ('live', 'silo_history', 'plex_history', 'import')),
	source_seq                INTEGER,      -- Silo lifecycle_seq; orders updates for one session
	account_id                INTEGER REFERENCES accounts(id),
	profile_id                TEXT,
	profile_name              TEXT,
	status                    TEXT NOT NULL CHECK (status IN ('active', 'stopped')),
	state                     TEXT,         -- playing, paused, buffering, stopped
	started_at                INTEGER NOT NULL,  -- unix ms
	ended_at                  INTEGER,
	updated_at                INTEGER NOT NULL,
	end_reason                TEXT,
	group_id                  INTEGER,

	media_type                TEXT,         -- movie, episode, track, ...
	item_id                   TEXT,         -- Silo content_id, Plex ratingKey
	parent_item_id            TEXT,         -- season / album
	grandparent_item_id       TEXT,         -- series / artist
	media_file_id             TEXT,
	title                     TEXT,
	series_title              TEXT,
	season_number             INTEGER,
	episode_number            INTEGER,
	episode_title             TEXT,
	year                      INTEGER,
	library_id                TEXT,
	library_name              TEXT,
	poster_url                TEXT,
	media_duration_s          REAL,

	start_position_s          REAL,
	end_position_s            REAL,
	played_s                  REAL,         -- measured watch time, no pauses or seeks
	paused_s                  REAL,
	percent_complete          REAL,
	watched                   INTEGER,

	client_name               TEXT,
	client_version            TEXT,
	client_product            TEXT,
	platform                  TEXT,
	platform_version          TEXT,
	device_id                 TEXT,
	device_name               TEXT,
	ip_address                TEXT,
	location                  TEXT CHECK (location IN ('lan', 'wan')),
	is_relayed                INTEGER,
	is_secure                 INTEGER,

	play_method               TEXT,         -- direct_play, direct_stream, transcode
	video_decision            TEXT,         -- direct, copy, transcode
	audio_decision            TEXT,
	subtitle_decision         TEXT,         -- direct, copy, transcode (burn), none
	transcode_hw_decode       TEXT,
	transcode_hw_encode       TEXT,
	transcode_speed           REAL,
	transcode_throttled       INTEGER,
	transcode_reason          TEXT,
	tone_mapping              TEXT,

	source_container          TEXT,
	source_bitrate_kbps       INTEGER,
	source_video_codec        TEXT,
	source_video_resolution   TEXT,
	source_video_width        INTEGER,
	source_video_height       INTEGER,
	source_video_dynamic_range TEXT,       -- SDR, HDR10, HDR10+, Dolby Vision, HLG
	source_audio_codec        TEXT,
	source_audio_channels     INTEGER,
	source_audio_language     TEXT,
	source_subtitle_codec     TEXT,
	source_subtitle_language  TEXT,

	stream_container          TEXT,
	stream_bitrate_kbps       INTEGER,
	stream_video_codec        TEXT,
	stream_video_resolution   TEXT,
	stream_video_width        INTEGER,
	stream_video_height       INTEGER,
	stream_audio_codec        TEXT,
	stream_audio_channels     INTEGER,
	bandwidth_kbps            INTEGER,
	quality_profile           TEXT,
	node                      TEXT,

	extra                     TEXT,         -- JSON for anything not worth a column yet
	UNIQUE (source, session_key)
);
CREATE INDEX plays_started ON plays(started_at);
CREATE INDEX plays_status ON plays(status) WHERE status = 'active';
CREATE INDEX plays_account ON plays(account_id, started_at);
CREATE INDEX plays_item ON plays(source, item_id);
CREATE INDEX plays_grandparent ON plays(source, grandparent_item_id);

-- State transitions within a play: start, pause, resume, buffer, stop,
-- decision changes. Feeds timelines and pause/buffer analytics.
CREATE TABLE play_events (
	id         INTEGER PRIMARY KEY,
	play_id    INTEGER NOT NULL REFERENCES plays(id) ON DELETE CASCADE,
	at         INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	position_s REAL,
	detail     TEXT
);
CREATE INDEX play_events_play ON play_events(play_id, at);

-- Per-minute concurrency and bandwidth, for peak-stream and bandwidth graphs.
CREATE TABLE stream_samples (
	at              INTEGER NOT NULL,
	source          TEXT NOT NULL,
	streams         INTEGER NOT NULL,
	direct_plays    INTEGER NOT NULL,
	direct_streams  INTEGER NOT NULL,
	transcodes      INTEGER NOT NULL,
	bandwidth_kbps  INTEGER NOT NULL,
	lan_kbps        INTEGER NOT NULL,
	wan_kbps        INTEGER NOT NULL,
	PRIMARY KEY (at, source)
) WITHOUT ROWID;

-- Reserved for IP geolocation; filled lazily once that feature exists.
CREATE TABLE ip_locations (
	ip         TEXT PRIMARY KEY,
	city       TEXT,
	region     TEXT,
	country    TEXT,
	latitude   REAL,
	longitude  REAL,
	asn        TEXT,
	looked_up  INTEGER NOT NULL
);

-- Ingest cursors and one-time import markers.
CREATE TABLE ingest_state (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
`,
}

// playColumns lists every plays column an ingester may set. upsertPlay only
// writes keys from this set, so a typo fails loudly instead of building SQL.
var playColumns = map[string]bool{}

func init() {
	for _, column := range strings.Fields(`source_seq account_id profile_id profile_name status state started_at
		ended_at end_reason group_id origin media_type item_id parent_item_id grandparent_item_id media_file_id title
		series_title season_number episode_number episode_title year library_id library_name poster_url media_duration_s
		start_position_s end_position_s played_s paused_s percent_complete watched client_name client_version
		client_product platform platform_version device_id device_name ip_address location is_relayed is_secure
		play_method video_decision audio_decision subtitle_decision transcode_hw_decode transcode_hw_encode
		transcode_speed transcode_throttled transcode_reason tone_mapping source_container source_bitrate_kbps
		source_video_codec source_video_resolution source_video_width source_video_height source_video_dynamic_range
		source_audio_codec source_audio_channels source_audio_language source_subtitle_codec source_subtitle_language
		stream_container stream_bitrate_kbps stream_video_codec stream_video_resolution stream_video_width
		stream_video_height stream_audio_codec stream_audio_channels bandwidth_kbps quality_profile node extra`) {
		playColumns[column] = true
	}
}

type store struct {
	db *sql.DB
	mu sync.Mutex // serializes multi-statement writes (account/person merges)
}

func openStore(directory string) (*store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "siloscope.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps memory low and makes SQLite's single writer explicit.
	db.SetMaxOpenConns(1)
	s := &store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this build supports (%d)", current, len(migrations))
	}
	for version := current + 1; version <= len(migrations); version++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[version-1]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, time.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) getState(key string) string {
	var value string
	_ = s.db.QueryRow(`SELECT value FROM ingest_state WHERE key = ?`, key).Scan(&value)
	return value
}

func (s *store) setState(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO ingest_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, time.Now().UnixMilli())
	return err
}

// playFields is a sparse set of plays columns. Nil values are skipped, so an
// update never erases something an earlier observation recorded.
type playFields map[string]any

func (fields playFields) set(column string, value any) {
	switch typed := value.(type) {
	case nil:
		return
	case string:
		if typed == "" {
			return
		}
	case *string:
		if typed == nil || *typed == "" {
			return
		}
		value = *typed
	case *int:
		if typed == nil {
			return
		}
		value = *typed
	case *int64:
		if typed == nil {
			return
		}
		value = *typed
	case *float64:
		if typed == nil {
			return
		}
		value = *typed
	case *bool:
		if typed == nil {
			return
		}
		value = boolInt(*typed)
	case bool:
		value = boolInt(typed)
	case time.Time:
		if typed.IsZero() {
			return
		}
		value = typed.UnixMilli()
	}
	fields[column] = value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// upsertPlay inserts the play or updates the given columns, returning its id.
// A stopped play is never reopened, and when both sides carry a source_seq a
// lower one is ignored so late events cannot roll state back.
func (s *store) upsertPlay(ctx context.Context, source, sessionKey string, fields playFields) (int64, bool, error) {
	columns := make([]string, 0, len(fields))
	for column := range fields {
		if !playColumns[column] {
			return 0, false, fmt.Errorf("unknown plays column %q", column)
		}
		columns = append(columns, column)
	}
	sort.Strings(columns)
	if _, ok := fields["started_at"]; !ok {
		fields["started_at"] = time.Now().UnixMilli()
		columns = append(columns, "started_at")
	}
	if _, ok := fields["status"]; !ok {
		fields["status"] = "active"
		columns = append(columns, "status")
	}
	now := time.Now().UnixMilli()
	insertColumns := append([]string{"source", "session_key", "updated_at"}, columns...)
	args := []any{source, sessionKey, now}
	updates := []string{"updated_at = excluded.updated_at"}
	for _, column := range columns {
		args = append(args, fields[column])
		switch column {
		case "started_at":
			// Earliest observation wins: a backfilled start is more accurate than first-seen.
			updates = append(updates, "started_at = MIN(plays.started_at, excluded.started_at)")
		case "status", "state", "ended_at", "end_reason":
			// Once stopped, only a final record (which also sets status=stopped) may change these.
			updates = append(updates, fmt.Sprintf("%[1]s = CASE WHEN plays.status = 'stopped' AND excluded.status <> 'stopped' THEN plays.%[1]s ELSE excluded.%[1]s END", column))
		default:
			updates = append(updates, fmt.Sprintf("%[1]s = excluded.%[1]s", column))
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(insertColumns)), ", ")
	query := fmt.Sprintf(`INSERT INTO plays (%s) VALUES (%s)
		ON CONFLICT(source, session_key) DO UPDATE SET %s
		WHERE excluded.source_seq IS NULL OR plays.source_seq IS NULL OR excluded.source_seq >= plays.source_seq
		RETURNING id`,
		strings.Join(insertColumns, ", "), placeholders, strings.Join(updates, ", "))
	var id int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// The WHERE clause rejected a stale update.
		return 0, false, nil
	}
	return id, true, err
}

func (s *store) addPlayEvent(ctx context.Context, playID int64, at time.Time, kind string, position *float64, detail string) {
	if playID == 0 {
		return
	}
	var detailValue any
	if detail != "" {
		detailValue = detail
	}
	var positionValue any
	if position != nil {
		positionValue = *position
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO play_events (play_id, at, kind, position_s, detail) VALUES (?, ?, ?, ?, ?)`, playID, at.UnixMilli(), kind, positionValue, detailValue)
}

// finalizeGroup assigns a stopped play to a resume group: the same account,
// profile and item, the previous attempt unwatched and ended within 12 hours,
// and this attempt starting near where that one stopped.
func (s *store) finalizeGroup(ctx context.Context, playID int64) {
	var accountID sql.NullInt64
	var source, itemID, profileID sql.NullString
	var startedAt int64
	var startPosition sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `SELECT source, account_id, profile_id, item_id, started_at, start_position_s FROM plays WHERE id = ?`, playID).
		Scan(&source, &accountID, &profileID, &itemID, &startedAt, &startPosition)
	if err != nil {
		return
	}
	group := playID
	if accountID.Valid && itemID.Valid && startPosition.Valid && startPosition.Float64 > 60 {
		var previousGroup sql.NullInt64
		_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(group_id, id) FROM plays
			WHERE id <> ? AND source = ? AND account_id = ? AND COALESCE(profile_id, '') = COALESCE(?, '') AND item_id = ?
			  AND status = 'stopped' AND COALESCE(watched, 0) = 0
			  AND ended_at BETWEEN ? AND ?
			  AND ABS(COALESCE(end_position_s, 0) - ?) <= 120
			ORDER BY ended_at DESC LIMIT 1`,
			playID, source, accountID, profileID, itemID, startedAt-12*3600*1000, startedAt+60*1000, startPosition.Float64).Scan(&previousGroup)
		if previousGroup.Valid {
			group = previousGroup.Int64
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE plays SET group_id = ? WHERE id = ?`, group, playID)
}

// recordStreamSample stores current concurrency per source. Called once a minute.
func (s *store) recordStreamSample(ctx context.Context, now time.Time) {
	minute := now.Truncate(time.Minute).UnixMilli()
	_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO stream_samples
		(at, source, streams, direct_plays, direct_streams, transcodes, bandwidth_kbps, lan_kbps, wan_kbps)
		SELECT ?, source, COUNT(*),
			SUM(play_method = 'direct_play'), SUM(play_method = 'direct_stream'), SUM(play_method = 'transcode'),
			COALESCE(SUM(COALESCE(bandwidth_kbps, stream_bitrate_kbps, source_bitrate_kbps)), 0),
			COALESCE(SUM(CASE WHEN location = 'lan' THEN COALESCE(bandwidth_kbps, stream_bitrate_kbps, source_bitrate_kbps) END), 0),
			COALESCE(SUM(CASE WHEN location = 'wan' THEN COALESCE(bandwidth_kbps, stream_bitrate_kbps, source_bitrate_kbps) END), 0)
		FROM plays WHERE status = 'active' GROUP BY source`, minute)
}

func (s *store) close() error {
	return s.db.Close()
}
