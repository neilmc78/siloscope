package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// A title page: one movie or show across both servers, keyed like the top
// lists (titleKey), so the same film on Silo and Plex is one title. It shows
// who watched it, how far each person got, and rewatches. A rewatch is a
// completed viewing of a movie or episode the person had already completed
// in the period.

type titleStats struct {
	Key      string         `json:"key"`
	Title    string         `json:"title"`
	Kind     string         `json:"kind"` // movie or show
	Poster   string         `json:"poster_url,omitempty"`
	Range    string         `json:"range"`
	Bucket   string         `json:"bucket"`
	Totals   titleTotals    `json:"totals"`
	OverTime []statsBucket  `json:"plays_over_time"`
	Methods  map[string]int `json:"methods"`
	Viewers  []titleViewer  `json:"viewers"`
	Episodes []titleEpisode `json:"episodes,omitempty"`
	Recent   []recentPlay   `json:"recent"`
}

type titleTotals struct {
	Plays        int     `json:"plays"`
	WatchSeconds float64 `json:"watch_seconds"`
	Viewers      int     `json:"viewers"`
	Completions  int     `json:"completions"`
	Rewatches    int     `json:"rewatches"`
}

type titleViewer struct {
	PersonID     int64   `json:"person_id"`
	Name         string  `json:"name"`
	Plays        int     `json:"plays"`
	WatchSeconds float64 `json:"watch_seconds"`
	// Movie: completed viewings. Show: distinct episodes completed.
	Completed   int      `json:"completed"`
	Rewatches   int      `json:"rewatches"`
	LastWatched int64    `json:"last_watched"`
	LastEpisode string   `json:"last_episode,omitempty"` // show: the latest episode played
	Progress    *float64 `json:"progress,omitempty"`     // movie: how far the latest viewing got
}

type titleEpisode struct {
	Label   string `json:"label"` // "S1 E2 · Pilot"
	Plays   int    `json:"plays"`
	Viewers int    `json:"viewers"`
}

const titleEpisodeLimit = 10

func (app *application) titleStatsJSON(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	if app.store == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "History storage is not configured")
		return
	}
	key := request.URL.Query().Get("key")
	if !strings.HasPrefix(key, "movie:") && !strings.HasPrefix(key, "show:") {
		writeJSONError(writer, http.StatusBadRequest, "A title key is required")
		return
	}
	r := parseStatsRange(request)
	plays, err := app.loadStatsPlays(request, r, request.URL.Query().Get("source"), 0)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Stats could not be read")
		return
	}
	matching := plays[:0]
	for _, play := range plays {
		if playKey, _, _ := titleKey(play); playKey == key {
			matching = append(matching, play)
		}
	}
	if len(matching) == 0 {
		writeJSONError(writer, http.StatusNotFound, "No plays of this title in this period")
		return
	}
	_ = json.NewEncoder(writer).Encode(buildTitleStats(key, matching, r))
}

// buildTitleStats summarises the plays of one title, oldest first.
func buildTitleStats(key string, plays []statsPlay, r statsRange) titleStats {
	_, name, isShow := titleKey(plays[0])
	result := titleStats{Key: key, Title: name, Kind: "movie", Range: r.key, Bucket: r.bucket,
		Methods: map[string]int{"direct_play": 0, "direct_stream": 0, "transcode": 0}, Viewers: []titleViewer{}}
	if isShow {
		result.Kind = "show"
	}
	type episodeEntry struct {
		titleEpisode
		season, number int64
		groups, people map[int64]bool
	}
	groups := map[int64]bool{}
	viewers := map[int64]*titleViewer{}
	viewerGroups := map[int64]map[int64]bool{}
	// Completed viewings per person and unit (the movie, or one episode),
	// counted once per resume group.
	completedGroups := map[int64]map[string]map[int64]bool{}
	episodes := map[string]*episodeEntry{}
	for _, play := range plays {
		if result.Poster == "" {
			result.Poster = play.poster
		}
		newGroup := !groups[play.group]
		groups[play.group] = true
		result.Totals.WatchSeconds += play.watchSeconds
		if _, ok := result.Methods[play.method]; newGroup && ok {
			result.Methods[play.method]++
		}
		unit := "movie"
		if isShow {
			label := strings.TrimSpace(strings.Trim(episodeLabel(play)+" · "+play.episodeTitle, " ·"))
			unit = strings.ToLower(label)
			if label != "" {
				episode := episodes[unit]
				if episode == nil {
					episode = &episodeEntry{titleEpisode: titleEpisode{Label: label}, season: play.season.Int64, number: play.episode.Int64,
						groups: map[int64]bool{}, people: map[int64]bool{}}
					episodes[unit] = episode
				}
				episode.groups[play.group] = true
				if play.personID != 0 {
					episode.people[play.personID] = true
				}
			}
		}
		if play.personID == 0 {
			continue
		}
		viewer := viewers[play.personID]
		if viewer == nil {
			viewer = &titleViewer{PersonID: play.personID, Name: play.personName}
			viewers[play.personID] = viewer
			viewerGroups[play.personID] = map[int64]bool{}
			completedGroups[play.personID] = map[string]map[int64]bool{}
		}
		viewerGroups[play.personID][play.group] = true
		viewer.WatchSeconds += play.watchSeconds
		if play.started >= viewer.LastWatched {
			viewer.LastWatched = play.started
			if isShow {
				viewer.LastEpisode = episodeLabel(play)
			} else if play.percent.Valid {
				progress := play.percent.Float64
				viewer.Progress = &progress
			} else {
				viewer.Progress = nil
			}
		}
		if play.watched {
			if completedGroups[play.personID][unit] == nil {
				completedGroups[play.personID][unit] = map[int64]bool{}
			}
			completedGroups[play.personID][unit][play.group] = true
		}
	}

	result.Totals.Plays = len(groups)
	for id, viewer := range viewers {
		viewer.Plays = len(viewerGroups[id])
		for _, completions := range completedGroups[id] {
			if isShow {
				viewer.Completed++
			} else {
				viewer.Completed += len(completions)
			}
			viewer.Rewatches += len(completions) - 1
			result.Totals.Completions += len(completions)
		}
		result.Totals.Rewatches += viewer.Rewatches
		result.Viewers = append(result.Viewers, *viewer)
	}
	result.Totals.Viewers = len(result.Viewers)
	sort.Slice(result.Viewers, func(i, j int) bool {
		if result.Viewers[i].LastWatched != result.Viewers[j].LastWatched {
			return result.Viewers[i].LastWatched > result.Viewers[j].LastWatched
		}
		return result.Viewers[i].Name < result.Viewers[j].Name
	})

	ranked := make([]*episodeEntry, 0, len(episodes))
	for _, episode := range episodes {
		episode.Plays, episode.Viewers = len(episode.groups), len(episode.people)
		ranked = append(ranked, episode)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Plays != b.Plays {
			return a.Plays > b.Plays
		}
		if a.season != b.season {
			return a.season < b.season
		}
		if a.number != b.number {
			return a.number < b.number
		}
		return a.Label < b.Label
	})
	for index, episode := range ranked {
		if index == titleEpisodeLimit {
			break
		}
		result.Episodes = append(result.Episodes, episode.titleEpisode)
	}

	result.OverTime = playsOverTime(plays, r, rangeStart(r, plays))
	result.Recent = recentPlays(plays, 20)
	return result
}
