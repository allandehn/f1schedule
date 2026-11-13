package schedule

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"f1schedule/internal/cache"
	"f1schedule/internal/openf1"
)

type fakeAPI struct {
	meetings map[int][]openf1.Meeting
	sessions map[int][]openf1.Session
	err      error
}

func (f fakeAPI) Meetings(year int) ([]openf1.Meeting, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.meetings[year], nil
}

func (f fakeAPI) Sessions(year int) ([]openf1.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sessions[year], nil
}

func liveRestriction() error {
	return &openf1.APIError{
		URL:        "https://api.openf1.org/v1/meetings?year=2026",
		StatusCode: http.StatusUnauthorized,
		Body:       `{"detail":"Live F1 session in progress. Global API access (including past sessions) is restricted to authenticated users until the session ends."}`,
	}
}

func rateLimited() error {
	return &openf1.APIError{
		URL:        "https://api.openf1.org/v1/sessions?year=2027",
		StatusCode: http.StatusTooManyRequests,
		Body:       `{"detail":"Rate limit exceeded. Max 3 requests/second.","error":"Too Many Requests"}`,
	}
}

func sampleWeekend(now time.Time) (openf1.Meeting, []openf1.Session) {
	meeting := openf1.Meeting{
		MeetingKey:  42,
		MeetingName: "Italian Grand Prix",
		Location:    "Monza",
		CountryName: "Italy",
	}
	sessions := []openf1.Session{
		{
			SessionKey:       1,
			SessionType:      "Practice",
			SessionName:      "Practice 1",
			DateStart:        now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			DateEnd:          now.Add(30 * time.Minute).UTC().Format(time.RFC3339),
			MeetingKey:       42,
			CircuitShortName: "Monza",
			GmtOffset:        "02:00:00",
		},
		{
			SessionKey:       2,
			SessionType:      "Practice",
			SessionName:      "Practice 2",
			DateStart:        now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
			DateEnd:          now.Add(3 * time.Hour).UTC().Format(time.RFC3339),
			MeetingKey:       42,
			CircuitShortName: "Monza",
			GmtOffset:        "02:00:00",
		},
	}
	return meeting, sessions
}

func TestSessionStatus(t *testing.T) {
	start := time.Date(2026, 6, 27, 10, 30, 0, 0, time.UTC)
	end := time.Date(2026, 6, 27, 11, 30, 0, 0, time.UTC)

	if got := sessionStatus(start.Add(-time.Hour), start, end); got != StatusUpcoming {
		t.Fatalf("before start: got %q", got)
	}
	if got := sessionStatus(start.Add(time.Minute), start, end); got != StatusOngoing {
		t.Fatalf("during session: got %q", got)
	}
	if got := sessionStatus(end, start, end); got != StatusCompleted {
		t.Fatalf("at end: got %q", got)
	}
}

func TestIsRaceWeekend(t *testing.T) {
	if !isRaceWeekend("Australian Grand Prix") {
		t.Fatal("expected grand prix to be a race weekend")
	}
	if isRaceWeekend("Pre-Season Testing") {
		t.Fatal("expected testing to be excluded")
	}
}

func TestActiveWeekendSavesCache(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	meeting, sessions := sampleWeekend(now)
	api := fakeAPI{
		meetings: map[int][]openf1.Meeting{2026: {meeting}},
		sessions: map[int][]openf1.Session{2026: sessions},
	}
	path := filepath.Join(t.TempDir(), "schedule.json")

	weekend, err := ActiveWeekend(api, path, now)
	if err != nil {
		t.Fatal(err)
	}
	if weekend.Cached {
		t.Fatal("live fetch should not be marked cached")
	}
	if !weekend.Ongoing {
		t.Fatal("expected ongoing weekend")
	}
	if weekend.CurrentOngoingSession() == nil {
		t.Fatal("expected a live session")
	}

	if _, err := cache.Load(path); err != nil {
		t.Fatalf("expected cache to be written: %v", err)
	}
}

func TestActiveWeekendFallsBackToCacheOnLiveRestriction(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	meeting, sessions := sampleWeekend(now)
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := cache.Save(path, map[int][]openf1.Meeting{
		2026: {meeting},
	}, map[int][]openf1.Session{
		2026: sessions,
	}); err != nil {
		t.Fatal(err)
	}

	weekend, err := ActiveWeekend(fakeAPI{err: liveRestriction()}, path, now)
	if err != nil {
		t.Fatal(err)
	}
	if !weekend.Cached {
		t.Fatal("expected cached weekend")
	}
	if weekend.CacheSavedAt.IsZero() {
		t.Fatal("expected cache timestamp")
	}
	if weekend.MeetingName != "Italian Grand Prix" {
		t.Fatalf("got %q", weekend.MeetingName)
	}
	live := weekend.CurrentOngoingSession()
	if live == nil {
		t.Fatal("expected live session from cached times")
	}
	if !strings.HasPrefix(live.Label, "Practice 1") {
		t.Fatalf("live session: %q", live.Label)
	}
}

func TestActiveWeekendFallsBackToCacheOnRateLimit(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	meeting, sessions := sampleWeekend(now)
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := cache.Save(path, map[int][]openf1.Meeting{
		2026: {meeting},
	}, map[int][]openf1.Session{
		2026: sessions,
	}); err != nil {
		t.Fatal(err)
	}

	weekend, err := ActiveWeekend(fakeAPI{err: rateLimited()}, path, now)
	if err != nil {
		t.Fatal(err)
	}
	if !weekend.Cached {
		t.Fatal("expected cached weekend")
	}
	if weekend.MeetingName != "Italian Grand Prix" {
		t.Fatalf("got %q", weekend.MeetingName)
	}
	if weekend.CurrentOngoingSession() == nil {
		t.Fatal("expected live session from cached times")
	}
}

func TestActiveWeekendLiveRestrictionWithoutCache(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	_, err := ActiveWeekend(fakeAPI{err: liveRestriction()}, filepath.Join(t.TempDir(), "missing.json"), now)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no cached schedule") {
		t.Fatalf("got %v", err)
	}
}

func TestCacheSavedDuringWeekend(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	weekend := &Weekend{
		Cached: true,
		Sessions: []SessionEvent{
			{Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
			{Start: now.Add(24 * time.Hour), End: now.Add(25 * time.Hour)},
		},
	}

	weekend.CacheSavedAt = now
	if !weekend.CacheSavedDuringWeekend(time.UTC) {
		t.Fatal("expected save during the weekend to count as current")
	}

	weekend.CacheSavedAt = now.Add(-8 * 24 * time.Hour)
	if weekend.CacheSavedDuringWeekend(time.UTC) {
		t.Fatal("expected a save from the previous week to be outside this weekend")
	}
}

func TestFinished(t *testing.T) {
	weekend := &Weekend{
		Sessions: []SessionEvent{
			{Status: StatusCompleted},
			{Status: StatusCompleted},
		},
	}
	if !weekend.Finished() {
		t.Fatal("expected finished weekend")
	}
	weekend.Sessions[1].Status = StatusUpcoming
	if weekend.Finished() {
		t.Fatal("did not expect an unfinished weekend")
	}
}

func TestActiveWeekendFallsBackToPastWeekendFromCache(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	meeting := openf1.Meeting{
		MeetingKey:  7,
		MeetingName: "Belgian Grand Prix",
		Location:    "Spa",
		CountryName: "Belgium",
	}
	sessions := []openf1.Session{
		{
			SessionKey:       1,
			SessionType:      "Race",
			SessionName:      "Race",
			DateStart:        now.Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339),
			DateEnd:          now.Add(-10*24*time.Hour + 2*time.Hour).UTC().Format(time.RFC3339),
			MeetingKey:       7,
			CircuitShortName: "Spa",
			GmtOffset:        "02:00:00",
		},
	}
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := cache.Save(path, map[int][]openf1.Meeting{
		2026: {meeting},
	}, map[int][]openf1.Session{
		2026: sessions,
	}); err != nil {
		t.Fatal(err)
	}

	weekend, err := ActiveWeekend(fakeAPI{err: liveRestriction()}, path, now)
	if err != nil {
		t.Fatal(err)
	}
	if weekend.MeetingName != "Belgian Grand Prix" {
		t.Fatalf("got %q", weekend.MeetingName)
	}
	if !weekend.Finished() {
		t.Fatal("expected the cached weekend to be in the past")
	}
}
