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
// library. Silo's disk figures describe whole filesystems, so libraries that
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
}

const libraryUsageInterval = 6 * time.Hour

func (a *application) runLibraryUsage(ctx context.Context) {
	a.refreshLibraryUsage(ctx)
	ticker := time.NewTicker(libraryUsageInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshLibraryUsage(ctx)
		}
	}
}

func (a *application) refreshLibraryUsage(ctx context.Context) {
	silo := &siloIngester{app: a}
	libraries := map[string]*libraryUsage{}
	order := []string{}
	err := silo.pages(ctx, "/api/v2/libraries", nil, func(raw json.RawMessage) error {
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
			order = append(order, item.ID)
		}
		return nil
	})
	if err != nil {
		log.Printf("library usage: %v", err)
		return
	}
	seen := map[string]bool{}
	for _, kind := range []string{"movie", "episode"} {
		err := silo.pages(ctx, "/api/v2/admin/catalog/items", url.Values{"type": {kind}}, func(raw json.RawMessage) error {
			var items []struct {
				Versions []struct {
					FileID    string `json:"file_id"`
					FileSize  int64  `json:"file_size"`
					LibraryID string `json:"library_id"`
				} `json:"versions"`
			}
			if err := json.Unmarshal(raw, &items); err != nil {
				return err
			}
			for _, item := range items {
				for _, version := range item.Versions {
					library := libraries[version.LibraryID]
					if library == nil || seen[version.FileID] {
						continue
					}
					seen[version.FileID] = true
					library.Bytes += version.FileSize
					library.Files++
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("library usage: %v", err)
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
	a.libraryUsage.mu.Unlock()
}

func (a *application) libraryUsageJSON(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	a.libraryUsage.mu.RLock()
	defer a.libraryUsage.mu.RUnlock()
	payload := struct {
		UpdatedAt *time.Time     `json:"updated_at,omitempty"`
		Libraries []libraryUsage `json:"libraries"`
	}{Libraries: a.libraryUsage.libraries}
	if !a.libraryUsage.updatedAt.IsZero() {
		payload.UpdatedAt = &a.libraryUsage.updatedAt
	}
	if payload.Libraries == nil {
		payload.Libraries = []libraryUsage{}
	}
	_ = json.NewEncoder(writer).Encode(payload)
}
