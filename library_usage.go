package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// Library usage totals the size of every file Silo has catalogued, per
// library, and keeps the catalog itself for library stats (catalog.go). Silo's disk figures describe whole filesystems, so libraries that
// share one NAS mount report identical numbers; the catalog says how much of
// that each library holds without walking the share with du.

type libraryUsage struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Paths []string `json:"paths"`
	Bytes int64    `json:"bytes"`
	Files int      `json:"files"`
}

type libraryUsageCache struct {
	mu        sync.RWMutex
	libraries []libraryUsage
	updatedAt time.Time
	lastError string
}

func (a *application) libraryUsageFailed(err error) {
	log.Printf("library usage: %v", err)
	a.libraryUsage.mu.Lock()
	a.libraryUsage.lastError = err.Error()
	a.libraryUsage.mu.Unlock()
}

const libraryUsageInterval = 6 * time.Hour

func (a *application) runLibraryUsage(ctx context.Context) {
	for {
		a.refreshLibraryUsage(ctx)
		// A failed refresh (Silo busy or restarting) tries again soon rather
		// than leaving the sizes empty for six hours.
		a.libraryUsage.mu.RLock()
		next := libraryUsageInterval
		if a.libraryUsage.lastError != "" {
			next = 10 * time.Minute
		}
		a.libraryUsage.mu.RUnlock()
		if sleepContext(ctx, next) != nil {
			return
		}
	}
}

func (a *application) refreshLibraryUsage(ctx context.Context) {
	silo := &siloIngester{app: a}
	libraries := map[string]*libraryUsage{}
	order := []string{}
	// listLibraries returns every library at once and refuses paging parameters.
	err := silo.list(ctx, "/api/v2/libraries", func(raw json.RawMessage) error {
		return readLibraries(raw, libraries, &order)
	})
	if err != nil {
		a.libraryUsageFailed(err)
		return
	}
	seen := map[string]bool{}
	var catalog []catalogItem
	posterSources := map[string]string{} // title ID -> Silo's signed poster URL
	for _, kind := range []string{"movie", "episode"} {
		err := silo.pages(ctx, "/api/v2/admin/catalog/items", url.Values{"type": {kind}}, func(raw json.RawMessage) error {
			var entries []siloCatalogEntry
			if err := json.Unmarshal(raw, &entries); err != nil {
				return err
			}
			for _, entry := range entries {
				item := entry.item()
				for _, version := range entry.Versions {
					library := libraries[version.LibraryID]
					if library == nil || seen[version.FileID] {
						continue
					}
					seen[version.FileID] = true
					library.Bytes += version.FileSize
					library.Files++
					item.Bytes += version.FileSize
				}
				if item.ContentID != "" && item.AddedAt > 0 {
					catalog = append(catalog, item)
				}
				if id := item.titleID(); id != "" && entry.PosterURL != "" && posterSources[id] == "" {
					posterSources[id] = entry.PosterURL
				}
			}
			return nil
		})
		if err != nil {
			a.libraryUsageFailed(err)
			return
		}
	}
	result := make([]libraryUsage, 0, len(order))
	for _, id := range order {
		result = append(result, *libraries[id])
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Bytes > result[j].Bytes })
	a.libraryUsage.mu.Lock()
	a.libraryUsage.libraries = result
	a.libraryUsage.updatedAt = time.Now()
	a.libraryUsage.lastError = ""
	a.libraryUsage.mu.Unlock()
	log.Printf("library usage: %d libraries, %d files totalled", len(result), len(seen))
	if a.store != nil {
		if err := a.store.replaceCatalog(ctx, catalog); err != nil {
			log.Printf("catalog: %v", err)
			return
		}
		sort.SliceStable(catalog, func(i, j int) bool { return catalog[i].AddedAt > catalog[j].AddedAt })
		a.refreshCatalogPosters(ctx, catalog, posterSources)
	}
}

func (a *application) libraryUsageJSON(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	a.libraryUsage.mu.RLock()
	defer a.libraryUsage.mu.RUnlock()
	payload := struct {
		UpdatedAt *time.Time     `json:"updated_at,omitempty"`
		Error     string         `json:"error,omitempty"`
		Libraries []libraryUsage `json:"libraries"`
	}{Libraries: a.libraryUsage.libraries, Error: a.libraryUsage.lastError}
	if !a.libraryUsage.updatedAt.IsZero() {
		payload.UpdatedAt = &a.libraryUsage.updatedAt
	}
	if payload.Libraries == nil {
		payload.Libraries = []libraryUsage{}
	}
	_ = json.NewEncoder(writer).Encode(payload)
}

func readLibraries(raw json.RawMessage, libraries map[string]*libraryUsage, order *[]string) error {
	var items []struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Paths   []string `json:"paths"`
		Enabled *bool    `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		if item.Enabled != nil && !*item.Enabled {
			continue
		}
		libraries[item.ID] = &libraryUsage{ID: item.ID, Name: item.Name, Type: item.Type, Paths: item.Paths}
		*order = append(*order, item.ID)
	}
	return nil
}
