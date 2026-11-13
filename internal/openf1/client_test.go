package openf1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsLiveRestriction(t *testing.T) {
	live := &APIError{
		URL:        "https://api.openf1.org/v1/meetings?year=2026",
		StatusCode: http.StatusUnauthorized,
		Body:       `{"detail":"Live F1 session in progress. Global API access (including past sessions) is restricted to authenticated users until the session ends."}`,
	}
	if !IsLiveRestriction(live) {
		t.Fatal("expected live restriction")
	}
	if !IsLiveRestriction(fmt.Errorf("wrap: %w", live)) {
		t.Fatal("expected wrapped live restriction")
	}

	other401 := &APIError{StatusCode: http.StatusUnauthorized, Body: `{"detail":"missing api key"}`}
	if IsLiveRestriction(other401) {
		t.Fatal("did not expect a generic 401 to count as live restriction")
	}
	if IsLiveRestriction(fmt.Errorf("fetch failed")) {
		t.Fatal("did not expect a plain error to count as live restriction")
	}
}

func TestClientMeetingsLiveRestriction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meetings" {
			t.Fatalf("path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Live F1 session in progress. Global API access is restricted"}`))
	}))
	t.Cleanup(srv.Close)

	client := &Client{http: srv.Client(), baseURL: srv.URL}
	_, err := client.Meetings(2026)
	if !IsLiveRestriction(err) {
		t.Fatalf("got %v", err)
	}
}

func TestClientSessionsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Session{{
			SessionKey:  1,
			SessionName: "Practice 1",
			MeetingKey:  10,
		}})
	}))
	t.Cleanup(srv.Close)

	client := &Client{http: srv.Client(), baseURL: srv.URL}
	sessions, err := client.Sessions(2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionName != "Practice 1" {
		t.Fatalf("got %+v", sessions)
	}
}

func TestIsRateLimited(t *testing.T) {
	limited := &APIError{
		URL:        "https://api.openf1.org/v1/sessions?year=2027",
		StatusCode: http.StatusTooManyRequests,
		Body:       `{"detail":"Rate limit exceeded. Max 3 requests/second."}`,
	}
	if !IsRateLimited(limited) {
		t.Fatal("expected rate limit")
	}
	if !IsRateLimited(fmt.Errorf("wrap: %w", limited)) {
		t.Fatal("expected wrapped rate limit")
	}
	if IsRateLimited(&APIError{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("did not expect a 401 to count as a rate limit")
	}
	if IsRateLimited(fmt.Errorf("fetch failed")) {
		t.Fatal("did not expect a plain error to count as a rate limit")
	}
}

func TestClientRetriesRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":"Rate limit exceeded. Max 3 requests/second."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Session{{
			SessionKey:  1,
			SessionName: "Practice 1",
			MeetingKey:  10,
		}})
	}))
	t.Cleanup(srv.Close)

	var delays []time.Duration
	client := &Client{
		http:     srv.Client(),
		baseURL:  srv.URL,
		interval: -1,
		sleep:    func(delay time.Duration) { delays = append(delays, delay) },
	}

	sessions, err := client.Sessions(2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionName != "Practice 1" {
		t.Fatalf("got %+v", sessions)
	}
	if calls != 2 {
		t.Fatalf("calls: got %d, want 2", calls)
	}
	if len(delays) != 1 || delays[0] != 2*time.Second {
		t.Fatalf("delays: got %v, want [2s]", delays)
	}
}

func TestClientGivesUpAfterRateLimitRetries(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":"Rate limit exceeded. Max 3 requests/second."}`))
	}))
	t.Cleanup(srv.Close)

	var delays []time.Duration
	client := &Client{
		http:     srv.Client(),
		baseURL:  srv.URL,
		interval: -1,
		sleep:    func(delay time.Duration) { delays = append(delays, delay) },
	}

	_, err := client.Meetings(2026)
	if !IsRateLimited(err) {
		t.Fatalf("got %v, want a rate limit error", err)
	}
	if calls != maxRateLimitAttempts {
		t.Fatalf("calls: got %d, want %d", calls, maxRateLimitAttempts)
	}
	if len(delays) != maxRateLimitAttempts-1 {
		t.Fatalf("delays: got %v, want %d entries", delays, maxRateLimitAttempts-1)
	}
	for _, delay := range delays {
		if delay != defaultRetryDelay {
			t.Fatalf("delay: got %v, want %v", delay, defaultRetryDelay)
		}
	}
}

func TestClientPacesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	now := time.Unix(0, 0)
	var delays []time.Duration
	client := &Client{
		http:     srv.Client(),
		baseURL:  srv.URL,
		interval: 400 * time.Millisecond,
		clock:    func() time.Time { return now },
		sleep: func(delay time.Duration) {
			delays = append(delays, delay)
			now = now.Add(delay)
		},
	}

	for i := 0; i < 3; i++ {
		if _, err := client.Meetings(2026); err != nil {
			t.Fatal(err)
		}
	}
	if len(delays) != 2 {
		t.Fatalf("delays: got %v, want 2 entries", delays)
	}
	for _, delay := range delays {
		if delay != 400*time.Millisecond {
			t.Fatalf("delay: got %v, want 400ms", delay)
		}
	}
}
