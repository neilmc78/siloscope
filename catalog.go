package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Silo's catalog, as the library walk (library_usage.go) last read it: what
// each library holds and when Silo added it. Library stats join it to plays
// by title (titleKey), so Plex plays of a title in a Silo library count too.

type catalogItem struct {
	ContentID     string
	Type          string // movie, episode
	Title         string
	SeriesID      string
	SeriesTitle   string
	SeasonNumber  sql.NullInt64
	EpisodeNumber sql.NullInt64
	Year          sql.NullInt64
	AddedAt       int64 // unix ms
	LibraryIDs    []string
	Bytes         int64
}

// titleKey is the movie's or show's key, as plays use (titleKey in stats.go).
func (item catalogItem) titleKey() (string, string, bool) {
	return titleKeyFor(item.Type, item.Title, item.SeriesTitle, item.EpisodeNumber.Valid)
}

// titleID is the content_id of the movie, or of an episode's series.
func (item catalogItem) titleID() string {
	if item.Type == "episode" {
		return item.SeriesID
	}
	return item.ContentID
}

// siloCatalogEntry is one listAdminCatalogItems item.
type siloCatalogEntry struct {
	ContentID     string   `json:"content_id"`
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Year          int64    `json:"year"`
	SeriesID      string   `json:"series_id"`
	SeriesTitle   string   `json:"series_title"`
	SeasonNumber  *int64   `json:"season_number"`
	EpisodeNumber *int64   `json:"episode_number"`
	LibraryIDs    []string `json:"library_ids"`
	AddedAt       string   `json:"added_at"`
	PosterURL     string   `json:"poster_url"` // signed, expiring; Silo builds from 19 Nov 2026 on
	Versions      []struct {
		FileID    string `json:"file_id"`
		FileSize  int64  `json:"file_size"`
		LibraryID string `json:"library_id"`
	} `json:"versions"`
}

func (entry siloCatalogEntry) item() catalogItem {
	item := catalogItem{ContentID: entry.ContentID, Type: entry.Type, Title: entry.Title, SeriesID: entry.SeriesID,
		SeriesTitle: entry.SeriesTitle, LibraryIDs: entry.LibraryIDs}
	if entry.SeasonNumber != nil {
		item.SeasonNumber = sql.NullInt64{Int64: *entry.SeasonNumber, Valid: true}
	}
	if entry.EpisodeNumber != nil {
		item.EpisodeNumber = sql.NullInt64{Int64: *entry.EpisodeNumber, Valid: true}
	}
	if entry.Year > 0 {
		item.Year = sql.NullInt64{Int64: entry.Year, Valid: true}
	}
	if added, err := time.Parse(time.RFC3339Nano, entry.AddedAt); err == nil {
		item.AddedAt = added.UnixMilli()
	}
	return item
}

// replaceCatalog swaps in a complete walk of the catalog.
func (s *store) replaceCatalog(ctx context.Context, items []catalogItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_items`); err != nil {
		return err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO catalog_items (content_id, type, title, series_id, series_title,
		season_number, episode_number, year, added_at, library_ids, bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, item := range items {
		if _, err := insert.ExecContext(ctx, item.ContentID, item.Type, item.Title, nullString(item.SeriesID), nullString(item.SeriesTitle),
			item.SeasonNumber, item.EpisodeNumber, item.Year, item.AddedAt, strings.Join(item.LibraryIDs, ","), item.Bytes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *store) loadCatalog(ctx context.Context) ([]catalogItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_id, type, title, COALESCE(series_id, ''), COALESCE(series_title, ''),
		season_number, episode_number, year, added_at, library_ids, bytes FROM catalog_items ORDER BY added_at DESC, content_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []catalogItem
	for rows.Next() {
		var item catalogItem
		var libraries string
		if err := rows.Scan(&item.ContentID, &item.Type, &item.Title, &item.SeriesID, &item.SeriesTitle, &item.SeasonNumber,
			&item.EpisodeNumber, &item.Year, &item.AddedAt, &libraries, &item.Bytes); err != nil {
			return nil, err
		}
		if libraries != "" {
			item.LibraryIDs = strings.Split(libraries, ",")
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

const recentlyAddedLimit = 12

// refreshCatalogPosters caches posters for the newest titles from the
// walk's own catalog pages: Silo signs a poster_url on each item (an
// episode's is its series poster). The URLs expire, so they are fetched
// during the walk that read them. A title without one is tried again on the
// next walk. (Silo's per-item page would need a viewer profile, which
// Siloscope's admin key has not.)
func (a *application) refreshCatalogPosters(ctx context.Context, items []catalogItem, sources map[string]string) {
	if a.store == nil || a.posters == nil {
		return
	}
	for _, id := range newestTitleIDs(items, recentlyAddedLimit) {
		var poster string
		err := a.store.db.QueryRowContext(ctx, `SELECT poster_url FROM catalog_posters WHERE content_id = ?`, id).Scan(&poster)
		if err == nil && poster != "" {
			continue
		}
		source := sources[id]
		if source == "" {
			continue
		}
		local := a.posters.localURL(ctx, source)
		if local == "" {
			log.Printf("catalog poster %s could not be cached", id)
			continue
		}
		_, _ = a.store.db.ExecContext(ctx, `INSERT OR REPLACE INTO catalog_posters (content_id, poster_url, fetched_at) VALUES (?, ?, ?)`,
			id, local, time.Now().UnixMilli())
	}
}

// newestTitleIDs lists the movies and series with the latest additions,
// newest first. items are newest first.
func newestTitleIDs(items []catalogItem, limit int) []string {
	var ids []string
	seen := map[string]bool{}
	for _, item := range items {
		id := item.titleID()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) == limit {
			break
		}
	}
	return ids
}

type libraryStat struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Titles        int     `json:"titles"`   // movies, or shows
	Episodes      int     `json:"episodes"` // TV libraries
	Bytes         int64   `json:"bytes"`
	AddedTitles   int     `json:"added_titles"` // movies, or shows with a new episode, in the period
	AddedEpisodes int     `json:"added_episodes"`
	Plays         int     `json:"plays"`
	WatchSeconds  float64 `json:"watch_seconds"`
	Viewers       int     `json:"viewers"`
	NeverPlayed   int     `json:"never_played"` // titles nobody has ever played
}

type recentTitle struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Subtitle string `json:"subtitle,omitempty"`
	Poster   string `json:"poster_url,omitempty"`
	Library  string `json:"library,omitempty"`
	AddedAt  int64  `json:"added_at"`
	Played   bool   `json:"played"` // someone has played it, so it has a title page
}

type unplayedTitle struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Library string `json:"library,omitempty"`
	AddedAt int64  `json:"added_at"`
	Bytes   int64  `json:"bytes"`
}

type addedBucket struct {
	Start time.Time `json:"t"`
	Items []int     `json:"items"` // per library, in libraries' order
}

type libraryStatsPayload struct {
	UpdatedAt     *time.Time      `json:"updated_at,omitempty"`
	Error         string          `json:"error,omitempty"`
	Range         string          `json:"range"`
	Bucket        string          `json:"bucket"`
	Libraries     []libraryStat   `json:"libraries"`
	AddedOverTime []addedBucket   `json:"added_over_time"`
	Recent        []recentTitle   `json:"recently_added"`
	NeverPlayed   []unplayedTitle `json:"never_played"`
}

const neverPlayedLimit = 10

func (app *application) libraryStatsJSON(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	if app.store == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "History storage is not configured")
		return
	}
	r := parseStatsRange(request)
	ctx := request.Context()
	items, err := app.store.loadCatalog(ctx)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Library stats could not be read")
		return
	}
	plays, err := app.loadStatsPlays(request, r, request.URL.Query().Get("source"), 0)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Library stats could not be read")
		return
	}
	played, playPosters, err := app.store.playedTitles(ctx)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Library stats could not be read")
		return
	}
	posters, err := app.store.catalogPosters(ctx)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Library stats could not be read")
		return
	}
	app.libraryUsage.mu.RLock()
	libraries := app.libraryUsage.libraries
	payload := libraryStatsPayload{Error: app.libraryUsage.lastError}
	if !app.libraryUsage.updatedAt.IsZero() {
		updated := app.libraryUsage.updatedAt
		payload.UpdatedAt = &updated
	}
	app.libraryUsage.mu.RUnlock()
	stats := buildLibraryStats(libraries, items, plays, played, posters, playPosters, r)
	stats.UpdatedAt, stats.Error = payload.UpdatedAt, payload.Error
	_ = json.NewEncoder(writer).Encode(stats)
}

// playedTitles lists every title anyone has ever played, on either server,
// and the latest poster its plays saved (from Silo or Plex session rows).
func (s *store) playedTitles(ctx context.Context) (map[string]bool, map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(media_type, ''), COALESCE(title, ''), COALESCE(series_title, ''),
		episode_number IS NOT NULL, COALESCE(poster_url, '') FROM plays ORDER BY started_at DESC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	keys, posters := map[string]bool{}, map[string]string{}
	for rows.Next() {
		var mediaType, title, series, poster string
		var hasEpisode bool
		if err := rows.Scan(&mediaType, &title, &series, &hasEpisode, &poster); err != nil {
			return nil, nil, err
		}
		key, _, _ := titleKeyFor(mediaType, title, series, hasEpisode)
		if key == "" {
			continue
		}
		keys[key] = true
		if posters[key] == "" && poster != "" {
			posters[key] = poster
		}
	}
	return keys, posters, rows.Err()
}

func (s *store) catalogPosters(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_id, poster_url FROM catalog_posters WHERE poster_url != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posters := map[string]string{}
	for rows.Next() {
		var id, poster string
		if err := rows.Scan(&id, &poster); err != nil {
			return nil, err
		}
		posters[id] = poster
	}
	return posters, rows.Err()
}

// buildLibraryStats joins the catalog (newest first) to the period's plays.
// posters are the catalog's cached posters by title ID; playPosters, by
// title key, stand in for a played title the catalog has no poster for.
func buildLibraryStats(libraries []libraryUsage, items []catalogItem, plays []statsPlay, played map[string]bool, posters, playPosters map[string]string, r statsRange) libraryStatsPayload {
	payload := libraryStatsPayload{Range: r.key, Bucket: r.bucket, Libraries: []libraryStat{}, AddedOverTime: []addedBucket{},
		Recent: []recentTitle{}, NeverPlayed: []unplayedTitle{}}
	index := map[string]int{}
	for _, library := range libraries {
		index[library.ID] = len(payload.Libraries)
		payload.Libraries = append(payload.Libraries, libraryStat{ID: library.ID, Name: library.Name, Type: library.Type, Bytes: library.Bytes})
	}
	libraryName := func(ids []string) string {
		for _, id := range ids {
			if position, ok := index[id]; ok {
				return payload.Libraries[position].Name
			}
		}
		return ""
	}

	from := r.from.UnixMilli()
	if r.from.IsZero() && len(items) > 0 {
		from = items[len(items)-1].AddedAt
	}
	type titleEntry struct {
		key, name, kind, id string
		libraries           map[string]bool
		bytes, latest       int64
		episodes            []catalogItem // newest first
	}
	titles := map[string]*titleEntry{}
	var titleOrder []*titleEntry
	addedTitles := map[string]map[string]bool{} // library -> titles with something new in the period
	added := map[int64][]int{}
	for _, item := range items {
		key, name, isShow := item.titleKey()
		if key == "" {
			continue
		}
		entry := titles[key]
		if entry == nil {
			entry = &titleEntry{key: key, name: name, kind: "movie", id: item.titleID(), libraries: map[string]bool{}, latest: item.AddedAt}
			if isShow {
				entry.kind = "show"
			}
			titles[key] = entry
			titleOrder = append(titleOrder, entry)
		}
		entry.bytes += item.Bytes
		if isShow {
			entry.episodes = append(entry.episodes, item)
		}
		newInRange := item.AddedAt >= from
		var bucket []int
		if newInRange {
			start := r.bucketStart(time.UnixMilli(item.AddedAt)).Unix()
			if added[start] == nil {
				added[start] = make([]int, len(payload.Libraries))
			}
			bucket = added[start]
		}
		for _, id := range item.LibraryIDs {
			position, ok := index[id]
			if !ok {
				continue
			}
			entry.libraries[id] = true
			library := &payload.Libraries[position]
			if isShow {
				library.Episodes++
			}
			if !newInRange {
				continue
			}
			bucket[position]++
			if isShow {
				library.AddedEpisodes++
			}
			if addedTitles[id] == nil {
				addedTitles[id] = map[string]bool{}
			}
			addedTitles[id][key] = true
		}
	}
	for _, entry := range titleOrder {
		for id := range entry.libraries {
			library := &payload.Libraries[index[id]]
			library.Titles++
			if !played[entry.key] {
				library.NeverPlayed++
			}
		}
	}
	for id, keys := range addedTitles {
		payload.Libraries[index[id]].AddedTitles = len(keys)
	}

	// Plays in the period, by the libraries holding each title.
	groups := map[int]map[int64]bool{}
	viewers := map[int]map[int64]bool{}
	for _, play := range plays {
		key, _, _ := titleKey(play)
		entry := titles[key]
		if entry == nil {
			continue
		}
		for id := range entry.libraries {
			position := index[id]
			if groups[position] == nil {
				groups[position], viewers[position] = map[int64]bool{}, map[int64]bool{}
			}
			groups[position][play.group] = true
			if play.personID != 0 {
				viewers[position][play.personID] = true
			}
			payload.Libraries[position].WatchSeconds += play.watchSeconds
		}
	}
	for position := range payload.Libraries {
		payload.Libraries[position].Plays = len(groups[position])
		payload.Libraries[position].Viewers = len(viewers[position])
	}

	if len(items) > 0 {
		start := time.UnixMilli(from).In(r.location)
		for t := r.bucketStart(start); !t.After(r.to); t = r.nextBucket(t) {
			counts := added[t.Unix()]
			if counts == nil {
				counts = make([]int, len(payload.Libraries))
			}
			payload.AddedOverTime = append(payload.AddedOverTime, addedBucket{Start: t, Items: counts})
		}
	}

	// Titles are in order of their newest addition.
	for _, entry := range titleOrder {
		if len(payload.Recent) == recentlyAddedLimit {
			break
		}
		recent := recentTitle{Key: entry.key, Title: entry.name, Kind: entry.kind, Poster: firstNonEmpty(posters[entry.id], playPosters[entry.key]), AddedAt: entry.latest, Played: played[entry.key],
			Library: libraryName(sortedKeys(entry.libraries))}
		if entry.kind == "show" {
			recent.Subtitle = newEpisodesLabel(entry.episodes)
		}
		payload.Recent = append(payload.Recent, recent)
	}
	var unplayed []*titleEntry
	for _, entry := range titleOrder {
		if !played[entry.key] && len(entry.libraries) > 0 {
			unplayed = append(unplayed, entry)
		}
	}
	sort.SliceStable(unplayed, func(i, j int) bool { return unplayed[i].bytes > unplayed[j].bytes })
	for _, entry := range unplayed {
		if len(payload.NeverPlayed) == neverPlayedLimit {
			break
		}
		added := entry.latest
		if len(entry.episodes) > 0 {
			added = entry.episodes[len(entry.episodes)-1].AddedAt
		}
		payload.NeverPlayed = append(payload.NeverPlayed, unplayedTitle{Key: entry.key, Title: entry.name, Kind: entry.kind,
			Library: libraryName(sortedKeys(entry.libraries)), AddedAt: added, Bytes: entry.bytes})
	}
	return payload
}

// newEpisodesLabel describes a show's latest addition: the episode, or how
// many arrived within a day of it (a season dropped at once). episodes are
// newest first.
func newEpisodesLabel(episodes []catalogItem) string {
	if len(episodes) == 0 {
		return ""
	}
	latest := episodes[0].AddedAt
	count := 0
	for _, episode := range episodes {
		if latest-episode.AddedAt > 24*time.Hour.Milliseconds() {
			break
		}
		count++
	}
	if count == 1 {
		play := statsPlay{season: episodes[0].SeasonNumber, episode: episodes[0].EpisodeNumber}
		if label := episodeLabel(play); label != "" {
			return label
		}
	}
	return plural(count, "new episode")
}

func plural(count int, word string) string {
	if count == 1 {
		return "1 " + word
	}
	return strconv.Itoa(count) + " " + word + "s"
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
