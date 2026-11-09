package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the image has no zoneinfo; stats bucket in the viewer's zone
)

// Stats are computed in Go from one read of the plays in range. A household
// server has thousands of plays, not millions, and one pass keeps every chart
// consistent with the same filters.
//
// A play is one resume group (Tautulli-style), so stopping and resuming a
// film counts once. Attempts under 30 seconds that were not marked watched
// are ignored, as Tautulli does by default.

const minimumPlaySeconds = 30

type statsPlay struct {
	id, group                  int64
	source                     string
	personID                   int64
	personName                 string
	mediaType                  string
	title, seriesTitle, poster string
	episodeTitle               string
	season, episode            sql.NullInt64
	started, ended             int64
	watchSeconds               float64
	method, platform, client   string
	device, ip, location       string
	status                     string
	watched                    bool
	percent                    sql.NullFloat64
}

type statsRange struct {
	key      string
	from, to time.Time
	bucket   string // day, week, month
	location *time.Location
}

func parseStatsRange(request *http.Request) statsRange {
	query := request.URL.Query()
	location, err := time.LoadLocation(query.Get("tz"))
	if err != nil || query.Get("tz") == "" {
		location = time.UTC
	}
	now := time.Now().In(location)
	result := statsRange{key: query.Get("range"), to: now, location: location, bucket: "day"}
	switch result.key {
	case "30d":
		result.from = now.AddDate(0, 0, -30)
	case "90d":
		result.from = now.AddDate(0, 0, -90)
	case "365d":
		result.from = now.AddDate(-1, 0, 0)
		result.bucket = "week"
	case "all":
		result.from = time.Time{}
		result.bucket = "month"
	default:
		result.key = "7d"
		result.from = now.AddDate(0, 0, -7)
	}
	return result
}

// bucketStart truncates t (in the viewer's zone) to its bucket.
func (r statsRange) bucketStart(t time.Time) time.Time {
	t = t.In(r.location)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, r.location)
	switch r.bucket {
	case "week":
		offset := (int(day.Weekday()) + 6) % 7 // weeks start on Monday
		return day.AddDate(0, 0, -offset)
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, r.location)
	}
	return day
}

func (r statsRange) nextBucket(t time.Time) time.Time {
	switch r.bucket {
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	}
	return t.AddDate(0, 0, 1)
}

// loadStatsPlays reads plays that started in range. personID 0 means everyone.
func (app *application) loadStatsPlays(request *http.Request, r statsRange, source string, personID int64) ([]statsPlay, error) {
	conditions := []string{"p.started_at >= ?"}
	args := []any{r.from.UnixMilli()}
	if source == "silo" || source == "plex" {
		conditions = append(conditions, "p.source = ?")
		args = append(args, source)
	}
	if personID > 0 {
		conditions = append(conditions, "a.person_id = ?")
		args = append(args, personID)
	}
	rows, err := app.store.db.QueryContext(request.Context(), `SELECT p.id, COALESCE(p.group_id, p.id), p.source,
			COALESCE(a.person_id, 0), COALESCE(pe.display_name, ''), COALESCE(p.media_type, ''),
			COALESCE(p.title, ''), COALESCE(p.series_title, ''), COALESCE(p.poster_url, ''), COALESCE(p.episode_title, ''),
			p.season_number, p.episode_number, p.started_at, COALESCE(p.ended_at, p.updated_at),
			COALESCE(p.played_s, CASE WHEN p.origin = 'plex_history' THEN 0
				ELSE MAX(0, (COALESCE(p.ended_at, p.updated_at) - p.started_at) / 1000.0 - COALESCE(p.paused_s, 0)) END),
			COALESCE(p.play_method, ''), COALESCE(NULLIF(p.platform, ''), p.client_product, ''), COALESCE(NULLIF(p.client_name, ''), p.client_product, ''),
			COALESCE(NULLIF(p.device_name, ''), p.device_id, ''), COALESCE(p.ip_address, ''), COALESCE(p.location, ''),
			p.status, COALESCE(p.watched, 0), p.percent_complete
		FROM plays p
		LEFT JOIN accounts a ON a.id = p.account_id
		LEFT JOIN people pe ON pe.id = a.person_id
		WHERE `+strings.Join(conditions, " AND ")+`
		ORDER BY p.started_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plays []statsPlay
	for rows.Next() {
		var play statsPlay
		if err := rows.Scan(&play.id, &play.group, &play.source, &play.personID, &play.personName, &play.mediaType,
			&play.title, &play.seriesTitle, &play.poster, &play.episodeTitle, &play.season, &play.episode,
			&play.started, &play.ended, &play.watchSeconds, &play.method, &play.platform, &play.client,
			&play.device, &play.ip, &play.location, &play.status, &play.watched, &play.percent); err != nil {
			return nil, err
		}
		if play.status != "active" && !play.watched && play.watchSeconds < minimumPlaySeconds {
			continue
		}
		plays = append(plays, play)
	}
	return plays, rows.Err()
}

type statsBucket struct {
	Start        time.Time `json:"t"`
	Plays        int       `json:"plays"`
	WatchSeconds float64   `json:"watch_seconds"`
}

type concurrencyBucket struct {
	Start      time.Time `json:"t"`
	Streams    int       `json:"streams"`
	Transcodes int       `json:"transcodes"`
}

type rankedTitle struct {
	Title  string `json:"title"`
	Poster string `json:"poster_url,omitempty"`
	Plays  int    `json:"plays"`
	Users  int    `json:"users"`
	people map[int64]bool
	groups map[int64]bool
}

type rankedPerson struct {
	PersonID     int64   `json:"person_id"`
	Name         string  `json:"name"`
	Plays        int     `json:"plays"`
	WatchSeconds float64 `json:"watch_seconds"`
}

type rankedName struct {
	Name  string `json:"name"`
	Plays int    `json:"plays"`
}

type statsPayload struct {
	Range      string              `json:"range"`
	From       *time.Time          `json:"from,omitempty"`
	To         time.Time           `json:"to"`
	Bucket     string              `json:"bucket"`
	Totals     statsTotals         `json:"totals"`
	OverTime   []statsBucket       `json:"plays_over_time"`
	Concurrent []concurrencyBucket `json:"concurrent"`
	Peak       concurrencyPeak     `json:"peak"`
	ByWeekday  [7]int              `json:"by_weekday"` // Monday first
	ByHour     [24]int             `json:"by_hour"`
	Methods    map[string]int      `json:"methods"`
	Platforms  []rankedName        `json:"platforms"`
	TopMovies  []*rankedTitle      `json:"top_movies"`
	TopShows   []*rankedTitle      `json:"top_shows"`
	PopMovies  []*rankedTitle      `json:"popular_movies"`
	PopShows   []*rankedTitle      `json:"popular_shows"`
	TopUsers   []rankedPerson      `json:"top_users"`
	Recent     []recentPlay        `json:"recent"`
	Person     *personDetail       `json:"person,omitempty"`
	Devices    []deviceSummary     `json:"devices,omitempty"`
	Addresses  []addressSummary    `json:"addresses,omitempty"`
}

type statsTotals struct {
	Plays        int     `json:"plays"`
	WatchSeconds float64 `json:"watch_seconds"`
	Users        int     `json:"users"`
	Titles       int     `json:"titles"`
	Devices      int     `json:"devices"`
}

type concurrencyPeak struct {
	Streams    int        `json:"streams"`
	Transcodes int        `json:"transcodes"`
	At         *time.Time `json:"at,omitempty"`
}

type recentPlay struct {
	ID           int64    `json:"id"`
	Source       string   `json:"source"`
	Title        string   `json:"title"`
	Subtitle     string   `json:"subtitle,omitempty"`
	Poster       string   `json:"poster_url,omitempty"`
	Person       string   `json:"person,omitempty"`
	Platform     string   `json:"platform,omitempty"`
	Method       string   `json:"method,omitempty"`
	StartedAt    int64    `json:"started_at"`
	WatchSeconds float64  `json:"watch_seconds"`
	Percent      *float64 `json:"percent_complete,omitempty"`
	Watched      bool     `json:"watched"`
	Active       bool     `json:"active"`
}

type personDetail struct {
	ID       int64         `json:"id"`
	Name     string        `json:"name"`
	Accounts []accountView `json:"accounts"`
}

type deviceSummary struct {
	Name     string `json:"name"`
	Platform string `json:"platform,omitempty"`
	Source   string `json:"source"`
	Plays    int    `json:"plays"`
	LastSeen int64  `json:"last_seen"`
}

type addressSummary struct {
	IP       string `json:"ip"`
	Location string `json:"location,omitempty"`
	Plays    int    `json:"plays"`
	LastSeen int64  `json:"last_seen"`
}

func titleKey(play statsPlay) (string, string, bool) {
	if play.seriesTitle != "" && (play.mediaType == "episode" || play.episode.Valid) {
		return "show:" + strings.ToLower(play.seriesTitle), play.seriesTitle, true
	}
	if play.mediaType == "movie" {
		return "movie:" + strings.ToLower(play.title), play.title, false
	}
	return "", "", false
}

func rankTitles(titles map[string]*rankedTitle, byUsers bool, limit int) []*rankedTitle {
	list := make([]*rankedTitle, 0, len(titles))
	for _, title := range titles {
		title.Plays = len(title.groups)
		title.Users = len(title.people)
		list = append(list, title)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if byUsers && a.Users != b.Users {
			return a.Users > b.Users
		}
		if a.Plays != b.Plays {
			return a.Plays > b.Plays
		}
		return a.Title < b.Title
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}

func (app *application) buildStats(request *http.Request, personID int64) (statsPayload, error) {
	r := parseStatsRange(request)
	source := request.URL.Query().Get("source")
	plays, err := app.loadStatsPlays(request, r, source, personID)
	if err != nil {
		return statsPayload{}, err
	}
	payload := statsPayload{Range: r.key, To: r.to, Bucket: r.bucket, Methods: map[string]int{"direct_play": 0, "direct_stream": 0, "transcode": 0}}
	if !r.from.IsZero() {
		from := r.from
		payload.From = &from
	} else if len(plays) > 0 {
		from := time.UnixMilli(plays[0].started).In(r.location)
		payload.From = &from
	}

	groups := map[int64]bool{}
	people := map[int64]bool{}
	titleKeys := map[string]bool{}
	devices := map[string]*deviceSummary{}
	addresses := map[string]*addressSummary{}
	buckets := map[int64]*statsBucket{}
	movies, shows := map[string]*rankedTitle{}, map[string]*rankedTitle{}
	persons := map[int64]*rankedPerson{}
	platforms := map[string]*rankedName{}
	platformGroups := map[string]map[int64]bool{}

	for _, play := range plays {
		started := time.UnixMilli(play.started).In(r.location)
		newGroup := !groups[play.group]
		groups[play.group] = true
		if play.personID != 0 {
			people[play.personID] = true
		}
		payload.Totals.WatchSeconds += play.watchSeconds

		bucketTime := r.bucketStart(started)
		bucket := buckets[bucketTime.Unix()]
		if bucket == nil {
			bucket = &statsBucket{Start: bucketTime}
			buckets[bucketTime.Unix()] = bucket
		}
		bucket.WatchSeconds += play.watchSeconds
		if newGroup {
			bucket.Plays++
			payload.ByWeekday[(int(started.Weekday())+6)%7]++
			payload.ByHour[started.Hour()]++
			if _, ok := payload.Methods[play.method]; ok {
				payload.Methods[play.method]++
			}
		}

		if key, name, isShow := titleKey(play); key != "" {
			titleKeys[key] = true
			target := movies
			if isShow {
				target = shows
			}
			entry := target[key]
			if entry == nil {
				entry = &rankedTitle{Title: name, people: map[int64]bool{}, groups: map[int64]bool{}}
				target[key] = entry
			}
			entry.groups[play.group] = true
			if play.personID != 0 {
				entry.people[play.personID] = true
			}
			if entry.Poster == "" {
				entry.Poster = play.poster
			}
		}

		if play.personID != 0 {
			person := persons[play.personID]
			if person == nil {
				person = &rankedPerson{PersonID: play.personID, Name: play.personName}
				persons[play.personID] = person
			}
			if newGroup {
				person.Plays++
			}
			person.WatchSeconds += play.watchSeconds
		}

		if platform := firstNonEmpty(play.platform, play.client); platform != "" {
			key := strings.ToLower(platform)
			if platforms[key] == nil {
				platforms[key] = &rankedName{Name: displayPlatform(platform)}
				platformGroups[key] = map[int64]bool{}
			}
			platformGroups[key][play.group] = true
		}

		if deviceName := firstNonEmpty(play.device, play.client); deviceName != "" {
			key := play.source + "|" + strings.ToLower(deviceName)
			device := devices[key]
			if device == nil {
				device = &deviceSummary{Name: deviceName, Platform: displayPlatform(play.platform), Source: play.source}
				devices[key] = device
			}
			device.Plays++
			device.LastSeen = max(device.LastSeen, play.started)
		}
		if play.ip != "" {
			address := addresses[play.ip]
			if address == nil {
				address = &addressSummary{IP: play.ip, Location: play.location}
				addresses[play.ip] = address
			}
			address.Plays++
			address.LastSeen = max(address.LastSeen, play.started)
		}
	}

	payload.Totals.Plays = len(groups)
	payload.Totals.Users = len(people)
	payload.Totals.Titles = len(titleKeys)
	payload.Totals.Devices = len(devices)

	// Every bucket in range, including empty ones, so lines do not skip gaps.
	if payload.From != nil {
		for t := r.bucketStart(*payload.From); !t.After(r.to); t = r.nextBucket(t) {
			if bucket := buckets[t.Unix()]; bucket != nil {
				payload.OverTime = append(payload.OverTime, *bucket)
			} else {
				payload.OverTime = append(payload.OverTime, statsBucket{Start: t})
			}
		}
	}
	payload.Concurrent, payload.Peak = concurrency(plays, r, payload.OverTime)

	for key, name := range platforms {
		name.Plays = len(platformGroups[key])
		payload.Platforms = append(payload.Platforms, *name)
	}
	sort.Slice(payload.Platforms, func(i, j int) bool {
		if payload.Platforms[i].Plays != payload.Platforms[j].Plays {
			return payload.Platforms[i].Plays > payload.Platforms[j].Plays
		}
		return payload.Platforms[i].Name < payload.Platforms[j].Name
	})
	if len(payload.Platforms) > 8 {
		payload.Platforms = payload.Platforms[:8]
	}
	payload.TopMovies = rankTitles(movies, false, 5)
	payload.PopMovies = rankTitles(movies, true, 5)
	payload.TopShows = rankTitles(shows, false, 5)
	payload.PopShows = rankTitles(shows, true, 5)
	for _, person := range persons {
		payload.TopUsers = append(payload.TopUsers, *person)
	}
	sort.Slice(payload.TopUsers, func(i, j int) bool { return payload.TopUsers[i].Plays > payload.TopUsers[j].Plays })
	if len(payload.TopUsers) > 5 {
		payload.TopUsers = payload.TopUsers[:5]
	}

	seenGroups := map[int64]bool{}
	for index := len(plays) - 1; index >= 0 && len(payload.Recent) < 10; index-- {
		play := plays[index]
		if seenGroups[play.group] {
			continue
		}
		seenGroups[play.group] = true
		entry := recentPlay{ID: play.id, Source: play.source, Title: firstNonEmpty(play.seriesTitle, play.title), Poster: play.poster,
			Person: play.personName, Platform: firstNonEmpty(play.platform, play.client), Method: play.method,
			StartedAt: play.started, WatchSeconds: play.watchSeconds, Watched: play.watched, Active: play.status == "active"}
		if play.seriesTitle != "" {
			label := ""
			if play.season.Valid && play.episode.Valid {
				label = "S" + strconv.FormatInt(play.season.Int64, 10) + " E" + strconv.FormatInt(play.episode.Int64, 10)
			}
			entry.Subtitle = strings.TrimSpace(strings.Trim(label+" · "+play.episodeTitle, " ·"))
		}
		if play.percent.Valid {
			entry.Percent = &play.percent.Float64
		}
		payload.Recent = append(payload.Recent, entry)
	}

	if personID > 0 {
		for _, device := range devices {
			payload.Devices = append(payload.Devices, *device)
		}
		sort.Slice(payload.Devices, func(i, j int) bool { return payload.Devices[i].LastSeen > payload.Devices[j].LastSeen })
		for _, address := range addresses {
			payload.Addresses = append(payload.Addresses, *address)
		}
		sort.Slice(payload.Addresses, func(i, j int) bool { return payload.Addresses[i].LastSeen > payload.Addresses[j].LastSeen })
	}
	return payload, nil
}

// concurrency sweeps play intervals for the peak number of simultaneous
// streams (and transcodes) in each bucket. It covers imported history too,
// since Silo's history carries start and end times.
func concurrency(plays []statsPlay, r statsRange, buckets []statsBucket) ([]concurrencyBucket, concurrencyPeak) {
	type event struct {
		at        int64
		delta     int
		transcode bool
	}
	events := make([]event, 0, len(plays)*2)
	now := time.Now().UnixMilli()
	for _, play := range plays {
		end := play.ended
		if play.status == "active" {
			end = now
		}
		if end <= play.started {
			continue
		}
		transcode := play.method == "transcode"
		events = append(events, event{play.started, 1, transcode}, event{end, -1, transcode})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at != events[j].at {
			return events[i].at < events[j].at
		}
		return events[i].delta < events[j].delta // ends before starts at the same instant
	})
	result := make([]concurrencyBucket, len(buckets))
	for index, bucket := range buckets {
		result[index].Start = bucket.Start
	}
	var peak concurrencyPeak
	streams, transcodes, cursor := 0, 0, 0
	bucketEnd := func(index int) int64 {
		if index+1 < len(buckets) {
			return buckets[index+1].Start.UnixMilli()
		}
		return r.nextBucket(buckets[index].Start).UnixMilli()
	}
	for index := range result {
		// The level carried into the bucket counts for it.
		result[index].Streams, result[index].Transcodes = streams, transcodes
		end := bucketEnd(index)
		for cursor < len(events) && events[cursor].at < end {
			event := events[cursor]
			streams += event.delta
			if event.transcode {
				transcodes += event.delta
			}
			if event.at >= buckets[index].Start.UnixMilli() {
				result[index].Streams = max(result[index].Streams, streams)
				result[index].Transcodes = max(result[index].Transcodes, transcodes)
			}
			if streams > peak.Streams {
				peak.Streams = streams
				at := time.UnixMilli(event.at).In(r.location)
				peak.At = &at
			}
			peak.Transcodes = max(peak.Transcodes, transcodes)
			cursor++
		}
	}
	return result, peak
}

func (app *application) statsJSON(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	if app.store == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "History storage is not configured")
		return
	}
	personID, _ := strconv.ParseInt(request.URL.Query().Get("person"), 10, 64)
	payload, err := app.buildStats(request, personID)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Stats could not be read")
		return
	}
	if personID > 0 {
		people, err := app.store.listPeople(request.Context())
		if err == nil {
			for _, person := range people {
				if person.ID == personID {
					payload.Person = &personDetail{ID: person.ID, Name: person.DisplayName, Accounts: person.Accounts}
				}
			}
		}
		if payload.Person == nil {
			writeJSONError(writer, http.StatusNotFound, "Person not found")
			return
		}
	}
	_ = json.NewEncoder(writer).Encode(payload)
}

type userSummary struct {
	PersonID     int64    `json:"person_id"`
	Name         string   `json:"name"`
	Sources      []string `json:"sources"`
	Plays        int      `json:"plays"`
	WatchSeconds float64  `json:"watch_seconds"`
	Titles       int      `json:"titles"`
	Devices      int      `json:"devices"`
	LastPlay     int64    `json:"last_play,omitempty"`
	LastActive   int64    `json:"last_active,omitempty"`
}

// usersJSON lists every person with their figures for the range. People
// with no plays in range are included so the list doubles as a directory.
func (app *application) usersJSON(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	if app.store == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "History storage is not configured")
		return
	}
	r := parseStatsRange(request)
	source := request.URL.Query().Get("source")
	plays, err := app.loadStatsPlays(request, r, source, 0)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Users could not be read")
		return
	}
	people, err := app.store.listPeople(request.Context())
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Users could not be read")
		return
	}
	summaries := map[int64]*userSummary{}
	list := make([]*userSummary, 0, len(people))
	for _, person := range people {
		summary := &userSummary{PersonID: person.ID, Name: person.DisplayName, Sources: []string{}}
		seen := map[string]bool{}
		for _, account := range person.Accounts {
			if source != "" && source != "all" && account.Source != source {
				continue
			}
			if !seen[account.Source] {
				seen[account.Source] = true
				summary.Sources = append(summary.Sources, account.Source)
			}
			if account.LastSeen != nil {
				summary.LastActive = max(summary.LastActive, *account.LastSeen)
			}
		}
		if len(summary.Sources) == 0 {
			continue
		}
		summaries[person.ID] = summary
		list = append(list, summary)
	}
	groups := map[int64]map[int64]bool{}
	titles := map[int64]map[string]bool{}
	devices := map[int64]map[string]bool{}
	for _, play := range plays {
		summary := summaries[play.personID]
		if summary == nil {
			continue
		}
		if groups[play.personID] == nil {
			groups[play.personID], titles[play.personID], devices[play.personID] = map[int64]bool{}, map[string]bool{}, map[string]bool{}
		}
		groups[play.personID][play.group] = true
		if key, _, _ := titleKey(play); key != "" {
			titles[play.personID][key] = true
		}
		if device := firstNonEmpty(play.device, play.client); device != "" {
			devices[play.personID][play.source+"|"+strings.ToLower(device)] = true
		}
		summary.WatchSeconds += play.watchSeconds
		summary.LastPlay = max(summary.LastPlay, play.started)
		summary.LastActive = max(summary.LastActive, play.started)
	}
	for id, summary := range summaries {
		summary.Plays = len(groups[id])
		summary.Titles = len(titles[id])
		summary.Devices = len(devices[id])
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Plays != list[j].Plays {
			return list[i].Plays > list[j].Plays
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	_ = json.NewEncoder(writer).Encode(list)
}

// displayPlatform capitalizes all-lowercase platform names ("android",
// "web" from Silo) so they read like Plex's ("Android"), leaving mixed-case
// names such as "tvOS" alone.
func displayPlatform(name string) string {
	if name == "" || name != strings.ToLower(name) {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
