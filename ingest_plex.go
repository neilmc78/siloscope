package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Plex ingestion.
//
// Plex pushes PlaySessionStateNotification frames over its notification
// websocket whenever a session starts, pauses, resumes, stops, or (about every
// ten seconds while playing) moves. Each frame carries the session key, state
// and position. Siloscope measures watch and pause time from those frames and
// reads /status/sessions only when something new appears, so stream details
// stay current without polling. A slow safety read catches anything missed.

type plexXMLSession struct {
	SessionKey           string  `xml:"sessionKey,attr"`
	RatingKey            string  `xml:"ratingKey,attr"`
	ParentRatingKey      string  `xml:"parentRatingKey,attr"`
	GrandparentRatingKey string  `xml:"grandparentRatingKey,attr"`
	Type                 string  `xml:"type,attr"`
	Title                string  `xml:"title,attr"`
	ParentTitle          string  `xml:"parentTitle,attr"`
	GrandparentTitle     string  `xml:"grandparentTitle,attr"`
	Index                *int    `xml:"index,attr"`
	ParentIndex          *int    `xml:"parentIndex,attr"`
	Year                 *int    `xml:"year,attr"`
	Duration             float64 `xml:"duration,attr"`
	ViewOffset           float64 `xml:"viewOffset,attr"`
	LibrarySectionID     string  `xml:"librarySectionID,attr"`
	LibrarySectionTitle  string  `xml:"librarySectionTitle,attr"`
	User                 struct {
		ID    string `xml:"id,attr"`
		Title string `xml:"title,attr"`
		Thumb string `xml:"thumb,attr"`
	} `xml:"User"`
	Player struct {
		Address             string `xml:"address,attr"`
		Device              string `xml:"device,attr"`
		MachineIdentifier   string `xml:"machineIdentifier,attr"`
		Platform            string `xml:"platform,attr"`
		PlatformVersion     string `xml:"platformVersion,attr"`
		Product             string `xml:"product,attr"`
		RemotePublicAddress string `xml:"remotePublicAddress,attr"`
		State               string `xml:"state,attr"`
		Title               string `xml:"title,attr"`
		Version             string `xml:"version,attr"`
		Local               string `xml:"local,attr"`
		Relayed             string `xml:"relayed,attr"`
		Secure              string `xml:"secure,attr"`
	} `xml:"Player"`
	Session struct {
		ID        string `xml:"id,attr"`
		Bandwidth *int64 `xml:"bandwidth,attr"`
		Location  string `xml:"location,attr"`
	} `xml:"Session"`
	Media     []plexXMLMedia `xml:"Media"`
	Transcode *struct {
		Throttled        string   `xml:"throttled,attr"`
		Speed            *float64 `xml:"speed,attr"`
		VideoDecision    string   `xml:"videoDecision,attr"`
		AudioDecision    string   `xml:"audioDecision,attr"`
		SubtitleDecision string   `xml:"subtitleDecision,attr"`
		Container        string   `xml:"container,attr"`
		VideoCodec       string   `xml:"videoCodec,attr"`
		AudioCodec       string   `xml:"audioCodec,attr"`
		AudioChannels    *int     `xml:"audioChannels,attr"`
		Width            *int     `xml:"width,attr"`
		Height           *int     `xml:"height,attr"`
		HWDecoding       string   `xml:"transcodeHwDecoding,attr"`
		HWEncoding       string   `xml:"transcodeHwEncoding,attr"`
		HWDecodingTitle  string   `xml:"transcodeHwDecodingTitle,attr"`
		HWEncodingTitle  string   `xml:"transcodeHwEncodingTitle,attr"`
	} `xml:"TranscodeSession"`
}

type plexXMLMedia struct {
	ID              string `xml:"id,attr"`
	Selected        string `xml:"selected,attr"`
	Bitrate         *int64 `xml:"bitrate,attr"`
	Container       string `xml:"container,attr"`
	VideoCodec      string `xml:"videoCodec,attr"`
	VideoResolution string `xml:"videoResolution,attr"`
	Width           *int   `xml:"width,attr"`
	Height          *int   `xml:"height,attr"`
	AudioCodec      string `xml:"audioCodec,attr"`
	AudioChannels   *int   `xml:"audioChannels,attr"`
	Parts           []struct {
		ID       string          `xml:"id,attr"`
		Decision string          `xml:"decision,attr"`
		Streams  []plexXMLStream `xml:"Stream"`
	} `xml:"Part"`
}

type plexXMLStream struct {
	Type         int    `xml:"streamType,attr"`
	Codec        string `xml:"codec,attr"`
	Decision     string `xml:"decision,attr"`
	Selected     string `xml:"selected,attr"`
	Default      string `xml:"default,attr"`
	Language     string `xml:"language,attr"`
	LanguageCode string `xml:"languageCode,attr"`
	Channels     *int   `xml:"channels,attr"`
	ColorTrc     string `xml:"colorTrc,attr"`
	DOVIPresent  string `xml:"DOVIPresent,attr"`
	DisplayTitle string `xml:"displayTitle,attr"`
	Location     string `xml:"location,attr"`
	Burn         string `xml:"burn,attr"`
}

type plexTracker struct {
	key        string
	playID     int64
	sessionKey string
	state      string
	offset     float64 // seconds
	at         time.Time
	played     float64
	paused     float64
	startPos   float64
	method     string
	duration   float64
}

type plexNotification struct {
	SessionKey string  `json:"sessionKey"`
	RatingKey  string  `json:"ratingKey"`
	ViewOffset float64 `json:"viewOffset"`
	State      string  `json:"state"`
}

type plexIngester struct {
	app       *application
	store     *store
	trackers  map[string]*plexTracker // by play key
	bySession map[string]string       // Plex sessionKey -> play key
	notify    chan []plexNotification
	connected atomic.Bool
	ownerID   string // plex.tv id of the server owner, who appears as account "1"
	mu        sync.Mutex
	lastError string
}

func newPlexIngester(app *application, s *store) *plexIngester {
	return &plexIngester{app: app, store: s, trackers: map[string]*plexTracker{}, bySession: map[string]string{}, notify: make(chan []plexNotification, 64)}
}

func (ingest *plexIngester) noteError(err error) {
	if err == nil {
		return
	}
	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	if message := err.Error(); message != ingest.lastError {
		ingest.lastError = message
		log.Printf("plex history: %v", err)
	}
}

func (ingest *plexIngester) get(ctx context.Context, path string, query url.Values, headers map[string]string) ([]byte, error) {
	upstream := *ingest.app.config.plexURL
	upstream.Path = strings.TrimRight(upstream.Path, "/") + path
	upstream.RawQuery = query.Encode()
	return ingest.do(ctx, upstream.String(), "application/xml", headers)
}

func (ingest *plexIngester) do(ctx context.Context, target, accept string, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Plex-Token", ingest.app.config.plexToken)
	request.Header.Set("X-Plex-Client-Identifier", "siloscope")
	request.Header.Set("X-Plex-Product", "Siloscope")
	request.Header.Set("Accept", accept)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	client := *ingest.app.client
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4*maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 4*maxResponseBytes {
		return nil, errors.New("Plex response too large")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", request.URL.Host+request.URL.Path, response.Status)
	}
	return body, nil
}

func (ingest *plexIngester) run(ctx context.Context) {
	if ingest.store.getState("plex.live_since") == "" {
		_ = ingest.store.setState("plex.live_since", strconv.FormatInt(time.Now().UnixMilli(), 10))
	}
	ingest.restoreTrackers(ctx)
	ingest.syncAccounts(ctx)
	go ingest.importHistory(ctx)
	go ingest.listen(ctx)
	ingest.reconcile(ctx, time.Now())

	timer := time.NewTimer(ingest.safetyInterval())
	defer timer.Stop()
	soon := time.NewTimer(time.Hour)
	soon.Stop()
	soonPending := false
	accounts := time.NewTicker(6 * time.Hour)
	defer accounts.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case batch := <-ingest.notify:
			now := time.Now()
			unknown := false
			for _, note := range batch {
				key, ok := ingest.bySession[note.SessionKey]
				tracker := ingest.trackers[key]
				if !ok || tracker == nil {
					unknown = unknown || note.State != "stopped"
					continue
				}
				ingest.observe(ctx, tracker, note.State, note.ViewOffset/1000, now)
				if note.State == "stopped" {
					unknown = true // confirm the end and pick up the final position
				}
			}
			if unknown && !soonPending {
				soon.Reset(time.Second)
				soonPending = true
			}
		case <-soon.C:
			soonPending = false
			ingest.reconcile(ctx, time.Now())
			timer.Reset(ingest.safetyInterval())
		case <-timer.C:
			ingest.reconcile(ctx, time.Now())
			timer.Reset(ingest.safetyInterval())
		case <-accounts.C:
			ingest.syncAccounts(ctx)
		}
	}
}

// safetyInterval is how long to wait before re-reading /status/sessions with
// no notification asking for it. It is a backstop, not the primary signal.
func (ingest *plexIngester) safetyInterval() time.Duration {
	switch {
	case !ingest.connected.Load():
		return 15 * time.Second
	case len(ingest.trackers) > 0:
		return time.Minute
	default:
		return 5 * time.Minute
	}
}

func (ingest *plexIngester) restoreTrackers(ctx context.Context) {
	rows, err := ingest.store.db.QueryContext(ctx, `SELECT id, session_key, COALESCE(state, 'playing'), COALESCE(end_position_s, 0), updated_at,
		COALESCE(played_s, 0), COALESCE(paused_s, 0), COALESCE(start_position_s, 0), COALESCE(play_method, ''), COALESCE(media_duration_s, 0)
		FROM plays WHERE source = 'plex' AND status = 'active'`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		tracker := &plexTracker{}
		var updated int64
		if rows.Scan(&tracker.playID, &tracker.key, &tracker.state, &tracker.offset, &updated, &tracker.played, &tracker.paused, &tracker.startPos, &tracker.method, &tracker.duration) == nil {
			tracker.at = time.UnixMilli(updated)
			ingest.trackers[tracker.key] = tracker
		}
	}
}

// observe advances a tracker's measurement. A position advance counts as
// watched when it fits playback at up to twice normal speed (plus jitter);
// anything larger is a seek. Time spent paused counts as paused.
func (ingest *plexIngester) observe(ctx context.Context, tracker *plexTracker, state string, offset float64, now time.Time) {
	elapsed := now.Sub(tracker.at).Seconds()
	if elapsed > 0 && elapsed < 6*3600 {
		switch tracker.state {
		case "playing":
			if delta := offset - tracker.offset; delta > 0 && delta <= 2*elapsed+5 {
				tracker.played += delta
			}
		case "paused":
			tracker.paused += elapsed
		}
	}
	position := offset
	if state != tracker.state && state != "" {
		kind := map[string]string{"paused": "pause", "buffering": "buffer", "stopped": "stop"}[state]
		if state == "playing" {
			kind = "resume"
		}
		if kind != "" {
			ingest.store.addPlayEvent(ctx, tracker.playID, now, kind, &position, "")
		}
		tracker.state = state
	}
	tracker.offset = offset
	tracker.at = now
	fields := playFields{}
	if state != "stopped" && state != "" {
		fields.set("state", state)
	}
	fields.set("end_position_s", offset)
	fields.set("played_s", math.Round(tracker.played))
	fields.set("paused_s", math.Round(tracker.paused))
	if tracker.duration > 0 {
		fields.set("percent_complete", math.Min(100, offset/tracker.duration*100))
	}
	_, _, err := ingest.store.upsertPlay(ctx, "plex", tracker.key, fields)
	ingest.noteError(err)
}

// reconcile reads /status/sessions, records new and changed sessions, and
// closes plays whose sessions are gone.
func (ingest *plexIngester) reconcile(ctx context.Context, now time.Time) {
	body, err := ingest.get(ctx, "/status/sessions", nil, nil)
	if err != nil {
		ingest.noteError(err)
		return
	}
	var container struct {
		Videos []plexXMLSession `xml:"Video"`
		Tracks []plexXMLSession `xml:"Track"`
	}
	if err := xml.Unmarshal(body, &container); err != nil {
		ingest.noteError(err)
		return
	}
	present := map[string]bool{}
	ingest.bySession = map[string]string{}
	for _, session := range append(container.Videos, container.Tracks...) {
		key := session.Session.ID
		if key == "" {
			key = "sk:" + session.SessionKey + ":" + session.RatingKey
		}
		if session.Player.State == "stopped" {
			continue
		}
		present[key] = true
		ingest.bySession[session.SessionKey] = key
		tracker := ingest.trackers[key]
		if tracker == nil {
			tracker = &plexTracker{key: key, sessionKey: session.SessionKey, state: session.Player.State, offset: session.ViewOffset / 1000, at: now, startPos: session.ViewOffset / 1000}
			ingest.trackers[key] = tracker
			id, err := ingest.recordSession(ctx, session, tracker, now, true)
			if err != nil {
				ingest.noteError(err)
				delete(ingest.trackers, key)
				continue
			}
			tracker.playID = id
			position := tracker.startPos
			ingest.store.addPlayEvent(ctx, id, now, "start", &position, "")
			continue
		}
		tracker.sessionKey = session.SessionKey
		if _, err := ingest.recordSession(ctx, session, tracker, now, false); err != nil {
			ingest.noteError(err)
		}
		ingest.observe(ctx, tracker, session.Player.State, session.ViewOffset/1000, now)
	}
	for key, tracker := range ingest.trackers {
		if !present[key] {
			ingest.finish(ctx, tracker, now)
			delete(ingest.trackers, key)
		}
	}
}

func (ingest *plexIngester) finish(ctx context.Context, tracker *plexTracker, now time.Time) {
	// The last notification is the best end time; a restart gap must not
	// count as watched, so fall back to now only for a fresh tracker.
	ended := tracker.at
	if ended.IsZero() || now.Sub(ended) < 30*time.Second {
		ended = now
	}
	fields := playFields{}
	fields.set("status", "stopped")
	fields.set("state", "stopped")
	fields.set("ended_at", ended)
	fields.set("end_reason", "stopped")
	fields.set("end_position_s", tracker.offset)
	fields.set("played_s", math.Round(tracker.played))
	fields.set("paused_s", math.Round(tracker.paused))
	if tracker.duration > 0 {
		percent := math.Min(100, tracker.offset/tracker.duration*100)
		fields.set("percent_complete", percent)
		fields.set("watched", percent >= plexWatchedPercent)
	}
	id, _, err := ingest.store.upsertPlay(ctx, "plex", tracker.key, fields)
	if err != nil {
		ingest.noteError(err)
		return
	}
	if tracker.state != "stopped" {
		position := tracker.offset
		ingest.store.addPlayEvent(ctx, id, ended, "stop", &position, "")
	}
	ingest.store.finalizeGroup(ctx, id)
}

// plexWatchedPercent matches Tautulli's default watched threshold.
const plexWatchedPercent = 85

var plexThumbUUID = regexp.MustCompile(`/users/([0-9a-fA-F]{8,})/avatar`)

func (ingest *plexIngester) recordSession(ctx context.Context, session plexXMLSession, tracker *plexTracker, now time.Time, created bool) (int64, error) {
	fields := playFields{}
	fields.set("status", "active")
	fields.set("state", session.Player.State)
	if created {
		fields.set("started_at", now)
		fields.set("start_position_s", session.ViewOffset/1000)
		ingest.addSourceDetails(ctx, session, fields)
	}
	if session.User.ID != "" {
		externalID := session.User.ID
		if ingest.ownerID != "" && externalID == ingest.ownerID {
			externalID = "1"
		}
		info := accountInfo{Source: "plex", ExternalID: externalID, DisplayName: session.User.Title, Thumb: session.User.Thumb}
		if match := plexThumbUUID.FindStringSubmatch(session.User.Thumb); match != nil {
			info.PlexUUID = match[1]
		}
		accountID, err := ingest.store.ensureAccount(ctx, info, now)
		if err != nil {
			return 0, err
		}
		fields.set("account_id", accountID)
	}
	fields.set("media_type", session.Type)
	fields.set("item_id", session.RatingKey)
	fields.set("parent_item_id", session.ParentRatingKey)
	fields.set("grandparent_item_id", session.GrandparentRatingKey)
	fields.set("year", session.Year)
	fields.set("library_id", session.LibrarySectionID)
	fields.set("library_name", session.LibrarySectionTitle)
	posterKey := session.RatingKey
	switch session.Type {
	case "episode":
		fields.set("title", session.GrandparentTitle)
		fields.set("series_title", session.GrandparentTitle)
		fields.set("episode_title", session.Title)
		fields.set("season_number", session.ParentIndex)
		fields.set("episode_number", session.Index)
		posterKey = firstNonEmpty(session.GrandparentRatingKey, session.RatingKey)
	case "track":
		fields.set("title", session.Title)
		fields.set("series_title", session.GrandparentTitle)
		fields.set("episode_title", session.ParentTitle)
		posterKey = firstNonEmpty(session.ParentRatingKey, session.RatingKey)
	default:
		fields.set("title", session.Title)
	}
	if _, err := strconv.ParseUint(posterKey, 10, 64); err == nil {
		fields.set("poster_url", "/api/plex/poster?id="+posterKey)
	}
	if session.Duration > 0 {
		tracker.duration = session.Duration / 1000
		fields.set("media_duration_s", tracker.duration)
	}
	fields.set("client_name", session.Player.Title)
	fields.set("client_product", session.Player.Product)
	fields.set("client_version", session.Player.Version)
	fields.set("platform", session.Player.Platform)
	fields.set("platform_version", session.Player.PlatformVersion)
	fields.set("device_id", session.Player.MachineIdentifier)
	fields.set("device_name", session.Player.Device)
	fields.set("ip_address", firstNonEmpty(session.Player.RemotePublicAddress, session.Player.Address))
	location := session.Session.Location
	if location == "" && session.Player.Local != "" {
		location = map[string]string{"1": "lan", "0": "wan"}[session.Player.Local]
	}
	if location == "lan" || location == "wan" {
		fields.set("location", location)
	}
	if session.Player.Relayed != "" {
		fields.set("is_relayed", session.Player.Relayed == "1")
	}
	if session.Player.Secure != "" {
		fields.set("is_secure", session.Player.Secure == "1")
	}
	fields.set("bandwidth_kbps", session.Session.Bandwidth)

	// Stream side: what the client receives.
	video, audio, subtitle := "", "", ""
	if len(session.Media) > 0 {
		media := selectedMedia(session.Media)
		fields.set("stream_container", media.Container)
		fields.set("stream_bitrate_kbps", media.Bitrate)
		fields.set("stream_video_codec", media.VideoCodec)
		fields.set("stream_video_resolution", media.VideoResolution)
		fields.set("stream_video_width", media.Width)
		fields.set("stream_video_height", media.Height)
		fields.set("stream_audio_codec", media.AudioCodec)
		fields.set("stream_audio_channels", media.AudioChannels)
		for _, part := range media.Parts {
			for _, stream := range part.Streams {
				if stream.Selected != "1" && stream.Type != 1 {
					continue
				}
				switch stream.Type {
				case 1:
					video = firstNonEmpty(video, stream.Decision)
				case 2:
					audio = firstNonEmpty(audio, stream.Decision)
				case 3:
					subtitle = firstNonEmpty(subtitle, stream.Decision)
					if stream.Burn == "1" {
						subtitle = "burn"
					}
				}
			}
		}
	}
	if transcode := session.Transcode; transcode != nil {
		video = firstNonEmpty(transcode.VideoDecision, video)
		audio = firstNonEmpty(transcode.AudioDecision, audio)
		subtitle = firstNonEmpty(transcode.SubtitleDecision, subtitle)
		fields.set("stream_container", transcode.Container)
		fields.set("stream_video_codec", transcode.VideoCodec)
		fields.set("stream_audio_codec", transcode.AudioCodec)
		fields.set("stream_audio_channels", transcode.AudioChannels)
		fields.set("stream_video_width", transcode.Width)
		fields.set("stream_video_height", transcode.Height)
		if transcode.Height != nil && video == "transcode" {
			fields.set("stream_video_resolution", heightLabel(*transcode.Height))
		}
		fields.set("transcode_speed", transcode.Speed)
		if transcode.Throttled != "" {
			fields.set("transcode_throttled", transcode.Throttled == "1")
		}
		if transcode.HWDecoding != "" {
			fields.set("transcode_hw_decode", firstNonEmpty(transcode.HWDecodingTitle, transcode.HWDecoding))
		}
		if transcode.HWEncoding != "" {
			fields.set("transcode_hw_encode", firstNonEmpty(transcode.HWEncodingTitle, transcode.HWEncoding))
		}
	}
	video, audio = normalizeDecision(video), normalizeDecision(audio)
	fields.set("video_decision", video)
	fields.set("audio_decision", audio)
	fields.set("subtitle_decision", normalizeDecision(subtitle))
	method := "direct_play"
	switch {
	case video == "transcode" || audio == "transcode":
		method = "transcode"
	case video == "copy" || audio == "copy":
		method = "direct_stream"
	}
	if session.Type == "track" && audio == "" && video == "" {
		method = "direct_play"
	}
	fields.set("play_method", method)
	if !created && tracker.method != "" && tracker.method != method {
		ingest.store.addPlayEvent(ctx, tracker.playID, now, "decision_change", nil, tracker.method+" -> "+method)
	}
	tracker.method = method
	id, _, err := ingest.store.upsertPlay(ctx, "plex", tracker.key, fields)
	return id, err
}

// addSourceDetails reads the library item once per play: the session's Media
// describes the stream sent to the client, not the file on disk.
func (ingest *plexIngester) addSourceDetails(ctx context.Context, session plexXMLSession, fields playFields) {
	if _, err := strconv.ParseUint(session.RatingKey, 10, 64); err != nil {
		return
	}
	body, err := ingest.get(ctx, "/library/metadata/"+session.RatingKey, nil, nil)
	if err != nil {
		return
	}
	var container struct {
		Items []struct {
			Media []plexXMLMedia `xml:"Media"`
		} `xml:",any"`
	}
	if xml.Unmarshal(body, &container) != nil || len(container.Items) == 0 || len(container.Items[0].Media) == 0 {
		return
	}
	media := container.Items[0].Media[0]
	// Match the version being played by part id when the session names it.
	if len(session.Media) > 0 && len(session.Media[0].Parts) > 0 {
		partID := session.Media[0].Parts[0].ID
		for _, candidate := range container.Items[0].Media {
			for _, part := range candidate.Parts {
				if part.ID == partID {
					media = candidate
				}
			}
		}
	}
	fields.set("source_container", media.Container)
	fields.set("source_bitrate_kbps", media.Bitrate)
	fields.set("source_video_codec", media.VideoCodec)
	fields.set("source_video_resolution", media.VideoResolution)
	fields.set("source_video_width", media.Width)
	fields.set("source_video_height", media.Height)
	fields.set("source_audio_codec", media.AudioCodec)
	fields.set("source_audio_channels", media.AudioChannels)
	// The session's selected streams say which audio/subtitle track is in use.
	selectedLanguage := map[int]string{}
	if len(session.Media) > 0 {
		for _, part := range session.Media[0].Parts {
			for _, stream := range part.Streams {
				if stream.Selected == "1" {
					selectedLanguage[stream.Type] = firstNonEmpty(stream.LanguageCode, stream.Language)
				}
			}
		}
	}
	for _, part := range media.Parts {
		for _, stream := range part.Streams {
			switch stream.Type {
			case 1:
				fields.set("source_video_dynamic_range", dynamicRange(stream))
			case 2:
				if stream.Selected == "1" || (selectedLanguage[2] == "" && stream.Default == "1") {
					fields.set("source_audio_codec", stream.Codec)
					fields.set("source_audio_channels", stream.Channels)
					fields.set("source_audio_language", firstNonEmpty(stream.LanguageCode, stream.Language))
				}
			case 3:
				if stream.Selected == "1" {
					fields.set("source_subtitle_codec", stream.Codec)
					fields.set("source_subtitle_language", firstNonEmpty(stream.LanguageCode, stream.Language))
				}
			}
		}
	}
	if fields["source_audio_language"] == nil {
		fields.set("source_audio_language", selectedLanguage[2])
	}
	if fields["source_subtitle_language"] == nil {
		fields.set("source_subtitle_language", selectedLanguage[3])
	}
}

func selectedMedia(media []plexXMLMedia) plexXMLMedia {
	for _, candidate := range media {
		if candidate.Selected == "1" {
			return candidate
		}
	}
	return media[0]
}

func dynamicRange(stream plexXMLStream) string {
	title := strings.ToLower(stream.DisplayTitle)
	switch {
	case stream.DOVIPresent == "1" || strings.Contains(title, "dolby vision") || strings.Contains(title, "dovi"):
		return "Dolby Vision"
	case strings.Contains(title, "hdr10+"):
		return "HDR10+"
	case stream.ColorTrc == "smpte2084" || strings.Contains(title, "hdr"):
		return "HDR10"
	case stream.ColorTrc == "arib-std-b67" || strings.Contains(title, "hlg"):
		return "HLG"
	}
	return "SDR"
}

func normalizeDecision(value string) string {
	switch strings.ToLower(value) {
	case "", "none":
		return ""
	case "directplay", "direct play", "direct":
		return "direct"
	case "copy", "remux", "directstream":
		return "copy"
	case "transcode":
		return "transcode"
	case "burn":
		return "burn"
	}
	return strings.ToLower(value)
}

func heightLabel(height int) string {
	switch {
	case height >= 2000:
		return "4k"
	case height >= 1000:
		return "1080"
	case height >= 700:
		return "720"
	case height >= 500:
		return "576"
	case height >= 400:
		return "480"
	}
	return "sd"
}

// listen holds the notification websocket open, reconnecting with backoff.
func (ingest *plexIngester) listen(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := ingest.listenOnce(ctx)
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

func (ingest *plexIngester) listenOnce(ctx context.Context) error {
	endpoint := *ingest.app.config.plexURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/:/websockets/notifications"
	endpoint.RawQuery = url.Values{"X-Plex-Token": {ingest.app.config.plexToken}}.Encode()
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	header := http.Header{}
	header.Set("X-Plex-Token", ingest.app.config.plexToken)
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	connection, response, err := dialer.DialContext(ctx, endpoint.String(), header)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		// Never log the URL: it carries the token.
		return errors.New("Plex notification socket unavailable")
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	ingest.connected.Store(true)
	connection.SetReadLimit(maxResponseBytes)
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(90 * time.Second))
	})
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				if connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					return
				}
			}
		}
	}()
	for {
		connection.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := connection.ReadMessage()
		if err != nil {
			return errors.New("Plex notification socket closed")
		}
		var message struct {
			Container struct {
				Type  string             `json:"type"`
				Plays []plexNotification `json:"PlaySessionStateNotification"`
			} `json:"NotificationContainer"`
		}
		if json.Unmarshal(raw, &message) != nil || message.Container.Type != "playing" || len(message.Container.Plays) == 0 {
			continue
		}
		select {
		case ingest.notify <- message.Container.Plays:
		case <-ctx.Done():
			return nil
		}
	}
}

// syncAccounts reads the server owner and the people it is shared with from
// plex.tv so each Plex account carries its plex.tv uuid for linking.
func (ingest *plexIngester) syncAccounts(ctx context.Context) {
	type plexUser struct {
		ID       json.Number `json:"id"`
		UUID     string      `json:"uuid"`
		Username string      `json:"username"`
		Title    string      `json:"title"`
		Email    string      `json:"email"`
		Thumb    string      `json:"thumb"`
	}
	var owner plexUser
	body, err := ingest.do(ctx, "https://plex.tv/api/v2/user", "application/json", nil)
	if err != nil {
		ingest.noteError(fmt.Errorf("plex.tv account lookup failed; linking falls back to avatar URLs: %w", err))
	} else if json.Unmarshal(body, &owner) == nil && owner.ID != "" {
		ingest.ownerID = owner.ID.String()
		_, err := ingest.store.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: "1", Username: owner.Username, DisplayName: firstNonEmpty(owner.Title, owner.Username), Email: owner.Email, PlexUUID: owner.UUID, Thumb: owner.Thumb}, time.Time{})
		ingest.noteError(err)
	}
	for _, endpoint := range []string{"https://plex.tv/api/v2/friends", "https://plex.tv/api/v2/home/users"} {
		body, err := ingest.do(ctx, endpoint, "application/json", nil)
		if err != nil {
			continue
		}
		var users []plexUser
		if json.Unmarshal(body, &users) != nil {
			var wrapped struct {
				Users []plexUser `json:"users"`
			}
			if json.Unmarshal(body, &wrapped) != nil {
				continue
			}
			users = wrapped.Users
		}
		for _, user := range users {
			if user.ID == "" || user.ID.String() == ingest.ownerID {
				continue
			}
			_, err := ingest.store.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: user.ID.String(), Username: user.Username, DisplayName: firstNonEmpty(user.Title, user.Username), Email: user.Email, PlexUUID: user.UUID, Thumb: user.Thumb}, time.Time{})
			ingest.noteError(err)
		}
	}
	ingest.noteError(ingest.store.autoLink(ctx))
}

// importHistory copies Plex's own watch log once, for plays before Siloscope
// started listening. Plex records only finished views (no duration, client or
// stream details), so imported rows carry what it has and count as watched.
func (ingest *plexIngester) importHistory(ctx context.Context) {
	if ingest.store.getState("plex.history.imported") != "" {
		return
	}
	liveSince, _ := strconv.ParseInt(ingest.store.getState("plex.live_since"), 10, 64)
	imported := 0
	for start := 0; ctx.Err() == nil; start += 500 {
		body, err := ingest.get(ctx, "/status/sessions/history/all", url.Values{"sort": {"viewedAt:desc"}},
			map[string]string{"X-Plex-Container-Start": strconv.Itoa(start), "X-Plex-Container-Size": "500"})
		if err != nil {
			ingest.noteError(fmt.Errorf("Plex history import paused: %w", err))
			return
		}
		var container struct {
			Size  int `xml:"size,attr"`
			Items []struct {
				HistoryKey           string `xml:"historyKey,attr"`
				RatingKey            string `xml:"ratingKey,attr"`
				ParentRatingKey      string `xml:"parentRatingKey,attr"`
				GrandparentRatingKey string `xml:"grandparentRatingKey,attr"`
				Type                 string `xml:"type,attr"`
				Title                string `xml:"title,attr"`
				ParentTitle          string `xml:"parentTitle,attr"`
				GrandparentTitle     string `xml:"grandparentTitle,attr"`
				Index                *int   `xml:"index,attr"`
				ParentIndex          *int   `xml:"parentIndex,attr"`
				ViewedAt             int64  `xml:"viewedAt,attr"`
				AccountID            string `xml:"accountID,attr"`
				DeviceID             string `xml:"deviceID,attr"`
				LibrarySectionID     string `xml:"librarySectionID,attr"`
			} `xml:",any"`
		}
		if err := xml.Unmarshal(body, &container); err != nil {
			ingest.noteError(err)
			return
		}
		for _, item := range container.Items {
			viewed := time.Unix(item.ViewedAt, 0)
			if item.ViewedAt == 0 || (liveSince > 0 && viewed.UnixMilli() >= liveSince) {
				continue
			}
			key := "history:" + firstNonEmpty(item.HistoryKey, item.RatingKey+":"+strconv.FormatInt(item.ViewedAt, 10)+":"+item.AccountID)
			fields := playFields{}
			fields.set("status", "stopped")
			fields.set("state", "stopped")
			fields.set("origin", "plex_history")
			fields.set("started_at", viewed)
			fields.set("ended_at", viewed)
			fields.set("watched", true)
			fields.set("percent_complete", 100.0)
			fields.set("media_type", item.Type)
			fields.set("item_id", item.RatingKey)
			fields.set("parent_item_id", item.ParentRatingKey)
			fields.set("grandparent_item_id", item.GrandparentRatingKey)
			fields.set("library_id", item.LibrarySectionID)
			fields.set("device_id", item.DeviceID)
			posterKey := item.RatingKey
			if item.Type == "episode" {
				fields.set("title", item.GrandparentTitle)
				fields.set("series_title", item.GrandparentTitle)
				fields.set("episode_title", item.Title)
				fields.set("season_number", item.ParentIndex)
				fields.set("episode_number", item.Index)
				posterKey = firstNonEmpty(item.GrandparentRatingKey, item.RatingKey)
			} else {
				fields.set("title", item.Title)
			}
			if _, err := strconv.ParseUint(posterKey, 10, 64); err == nil {
				fields.set("poster_url", "/api/plex/poster?id="+posterKey)
			}
			if item.AccountID != "" {
				accountID, err := ingest.store.ensureAccount(ctx, accountInfo{Source: "plex", ExternalID: item.AccountID}, viewed)
				if err == nil {
					fields.set("account_id", accountID)
				}
			}
			if _, _, err := ingest.store.upsertPlay(ctx, "plex", key, fields); err != nil {
				ingest.noteError(err)
				return
			}
			imported++
		}
		if len(container.Items) < 500 {
			break
		}
	}
	if ctx.Err() == nil {
		_ = ingest.store.setState("plex.history.imported", strconv.FormatInt(time.Now().UnixMilli(), 10))
		log.Printf("plex history: imported %d earlier views", imported)
	}
}
