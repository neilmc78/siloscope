package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Silo poster URLs are either signed routes relative to Silo itself
// ("/api/...?...") or direct object-storage URLs, and the signatures expire.
// A browser resolves a relative one against Siloscope's address, so it never
// loaded. Siloscope fetches each poster once through Silo's address and serves
// it from its data directory; history keeps its posters after the signature
// lapses. The served route only reads cached files by hash.

type posterCache struct {
	app       *application
	directory string
	mu        sync.Mutex
	failed    map[string]time.Time
}

const maxPosterBytes = 2 << 20

var posterKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func newPosterCache(app *application, directory string) (*posterCache, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	return &posterCache{app: app, directory: directory, failed: map[string]time.Time{}}, nil
}

// posterKey identifies an image by its URL without the query, which holds
// the expiring signature. An episode's series poster gets one key across
// episodes and across signatures.
func posterKey(raw string) string {
	base := raw
	if index := strings.IndexByte(base, '?'); index >= 0 {
		base = base[:index]
	}
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:16])
}

func (cache *posterCache) path(key string) string {
	return filepath.Join(cache.directory, key)
}

// localURL returns the Siloscope URL for a Silo poster, fetching it first if
// it is not cached. It returns "" when the poster cannot be fetched.
func (cache *posterCache) localURL(ctx context.Context, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "/api/silo/poster?") {
		return raw
	}
	key := posterKey(raw)
	if _, err := os.Stat(cache.path(key)); err == nil {
		return "/api/silo/poster?key=" + key
	}
	cache.mu.Lock()
	if failedAt, ok := cache.failed[key]; ok && time.Since(failedAt) < 10*time.Minute {
		cache.mu.Unlock()
		return ""
	}
	cache.mu.Unlock()
	if err := cache.fetch(ctx, raw, key); err != nil {
		cache.mu.Lock()
		cache.failed[key] = time.Now()
		cache.mu.Unlock()
		return ""
	}
	return "/api/silo/poster?key=" + key
}

func (cache *posterCache) fetch(ctx context.Context, raw, key string) error {
	target, err := url.Parse(raw)
	if err != nil {
		return err
	}
	sendKey := false
	switch {
	case target.Scheme == "" && target.Host == "" && strings.HasPrefix(target.Path, "/"):
		// A route on Silo itself: resolve against SILO_URL and authenticate.
		base := *cache.app.config.siloURL
		base.Path = strings.TrimRight(base.Path, "/") + target.Path
		base.RawQuery = target.RawQuery
		target = &base
		sendKey = true
	case target.Scheme == "http" || target.Scheme == "https":
		// Presigned object storage: the URL is its own credential.
	default:
		return errors.New("unsupported poster URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "image/webp, image/jpeg, image/png")
	if sendKey {
		request.Header.Set("Authorization", "Bearer "+cache.app.config.apiKey)
	}
	client := *cache.app.client
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		// Never carry the Silo key to another host.
		if next.URL.Host != via[0].URL.Host {
			next.Header.Del("Authorization")
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	contentType := strings.Split(response.Header.Get("Content-Type"), ";")[0]
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(contentType, "image/") {
		return errors.New("poster unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPosterBytes+1))
	if err != nil || len(body) > maxPosterBytes {
		return errors.New("poster too large")
	}
	temporary, err := os.CreateTemp(cache.directory, ".poster-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), cache.path(key))
}

func (a *application) siloPoster(writer http.ResponseWriter, request *http.Request) {
	key := request.URL.Query().Get("key")
	if a.posters == nil || !posterKeyPattern.MatchString(key) {
		http.NotFound(writer, request)
		return
	}
	file, err := os.Open(a.posters.path(key))
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer file.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	contentType := http.DetectContentType(head[:n])
	if !strings.HasPrefix(contentType, "image/") {
		http.NotFound(writer, request)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = io.Copy(writer, file)
}

// siloSessionsJSON serves Silo's live sessions with poster URLs pointing at
// the poster cache, so the Playing tab can load them.
func (a *application) siloSessionsJSON(writer http.ResponseWriter, request *http.Request) {
	body, contentType, err := a.fetch(request.Context(), "sessions", "/api/v2/admin/sessions", a.config.cacheTTL)
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, "Silo is unavailable")
		return
	}
	if a.posters != nil {
		var sessions []map[string]any
		if json.Unmarshal(body, &sessions) == nil {
			for _, session := range sessions {
				if raw, ok := session["poster_url"].(string); ok && raw != "" {
					if local := a.posters.localURL(request.Context(), raw); local != "" {
						session["poster_url"] = local
					} else {
						delete(session, "poster_url")
					}
				}
			}
			if rewritten, err := json.Marshal(sessions); err == nil {
				body = rewritten
			}
		}
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", contentType)
	_, _ = writer.Write(body)
}
