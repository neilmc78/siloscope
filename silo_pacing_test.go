package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestSiloRequestsArePacedAndRetryAfter429(t *testing.T) {
	var calls, refused atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Refuse the first two requests the way Silo's limiter does.
		if calls.Add(1) <= 2 {
			refused.Add(1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"items":[],"page":{"has_more":false}}`))
	}))
	defer server.Close()
	siloURL, _ := url.Parse(server.URL)
	app := newApplication(config{siloURL: siloURL, apiKey: "key", timeout: 5 * time.Second})
	silo := &siloIngester{app: app}

	if _, status, err := silo.request(context.Background(), http.MethodGet, "/api/v2/admin/catalog/items", nil, "key"); err != nil || status != http.StatusOK {
		t.Fatalf("request after two 429s = %d, %v", status, err)
	}
	if refused.Load() != 2 || calls.Load() != 3 {
		t.Fatalf("calls = %d, refused = %d; want two retries", calls.Load(), refused.Load())
	}

	// After the burst, requests are spaced at the pacer's rate, so 25 more
	// take at least (25 - burst) / rate seconds instead of arriving at once.
	started := time.Now()
	for range 25 {
		if _, _, err := silo.request(context.Background(), http.MethodGet, "/api/v2/admin/catalog/items", nil, "key"); err != nil {
			t.Fatal(err)
		}
	}
	minimum := time.Duration(float64(25-siloBurst) / siloRequestsPerSecond * float64(time.Second))
	if elapsed := time.Since(started); elapsed < minimum-100*time.Millisecond {
		t.Fatalf("25 requests took %v; pacing should hold them to at least %v", elapsed, minimum)
	}
}
