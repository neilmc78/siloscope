package main

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Silo limits each API key (20 requests a second on the standard tier, 1200
// a minute) and each account. Siloscope's history import, catalog walk,
// account sync and live polling all share one key, so every request to Silo
// passes through one pacer that keeps Siloscope at half that rate, leaving
// room for anything else using the key.

const (
	siloRequestsPerSecond = 10
	siloBurst             = 10
	maxSiloRetries        = 3
)

type requestPacer struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	rate   float64
	burst  float64
}

func newRequestPacer(perSecond, burst int) *requestPacer {
	return &requestPacer{tokens: float64(burst), last: time.Now(), rate: float64(perSecond), burst: float64(burst)}
}

// wait blocks until a request may be sent or the context ends.
func (pacer *requestPacer) wait(ctx context.Context) error {
	for {
		pacer.mu.Lock()
		now := time.Now()
		pacer.tokens = min(pacer.burst, pacer.tokens+now.Sub(pacer.last).Seconds()*pacer.rate)
		pacer.last = now
		if pacer.tokens >= 1 {
			pacer.tokens--
			pacer.mu.Unlock()
			return nil
		}
		delay := time.Duration((1 - pacer.tokens) / pacer.rate * float64(time.Second))
		pacer.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// retryAfter reads a 429's Retry-After in seconds, falling back to an
// exponential delay, and caps the wait at 30 seconds.
func retryAfter(response *http.Response, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, 30*time.Second)
	}
	return min(time.Second<<attempt, 30*time.Second)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// doSilo sends a request to Silo through the pacer, retrying a 429 after
// Silo's Retry-After. build must return a fresh request for each attempt.
func (a *application) doSilo(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := a.siloPacer.wait(ctx); err != nil {
			return nil, err
		}
		request, err := build()
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err != nil || response.StatusCode != http.StatusTooManyRequests || attempt >= maxSiloRetries {
			return response, err
		}
		delay := retryAfter(response, attempt)
		response.Body.Close()
		if err := sleepContext(ctx, delay); err != nil {
			return nil, err
		}
	}
}
