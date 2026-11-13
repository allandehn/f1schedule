package schedule

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"f1schedule/internal/cache"
	"f1schedule/internal/openf1"
)

type Fetcher interface {
	Meetings(year int) ([]openf1.Meeting, error)
	Sessions(year int) ([]openf1.Session, error)
}

type SessionStatus string

const (
	StatusUpcoming  SessionStatus = "upcoming"
	StatusOngoing   SessionStatus = "ongoing"
	StatusCompleted SessionStatus = "completed"
)

type SessionEvent struct {
	Label  string
	Status SessionStatus
	Start  time.Time
	End    time.Time
}

type Weekend struct {
	MeetingName  string
	Location     string
	Country      string
	Circuit      string
	GmtOffset    string
	Sessions     []SessionEvent
	Ongoing      bool
	Cached       bool
	CacheSavedAt time.Time
}

func ActiveWeekend(client Fetcher, cachePath string, now time.Time) (*Weekend, error) {
	years := []int{now.Year()}
	if now.Month() >= time.November {
		years = append(years, now.Year()+1)
	}

	meetingsByYear, sessionsByYear, err := fetchAll(client, years)
	if err != nil {
		var reason string
		switch {
		case openf1.IsLiveRestriction(err):
			reason = "API locked during live session"
		case openf1.IsRateLimited(err):
			reason = "OpenF1 rate limit exceeded"
		default:
			return nil, err
		}
		if cachePath == "" {
			return nil, fmt.Errorf("%s and no cache path configured: %w", reason, err)
		}
		snap, loadErr := cache.Load(cachePath)
		if loadErr != nil {
			return nil, fmt.Errorf("%s and no cached schedule: %v", reason, loadErr)
		}
		weekend, buildErr := weekendFromData(flatten(snap.MeetingsByYear()), flatten(snap.SessionsByYear()), now, true)
		if buildErr != nil {
			return nil, buildErr
		}
		weekend.Cached = true
		weekend.CacheSavedAt = snap.SavedAt
		return weekend, nil
	}

	if cachePath != "" {
		if saveErr := cache.Save(cachePath, meetingsByYear, sessionsByYear); saveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save schedule cache: %v\n", saveErr)
		}
	}

	return weekendFromData(flatten(meetingsByYear), flatten(sessionsByYear), now, false)
}

func fetchAll(client Fetcher, years []int) (map[int][]openf1.Meeting, map[int][]openf1.Session, error) {
	meetingsByYear := make(map[int][]openf1.Meeting, len(years))
	sessionsByYear := make(map[int][]openf1.Session, len(years))
	for _, year := range years {
		meetings, err := client.Meetings(year)
		if err != nil {
			return nil, nil, err
		}
		sessions, err := client.Sessions(year)
		if err != nil {
			return nil, nil, err
		}
		meetingsByYear[year] = meetings
		sessionsByYear[year] = sessions
	}
	return meetingsByYear, sessionsByYear, nil
}

func flatten[T any](byYear map[int][]T) []T {
	var out []T
	for _, list := range byYear {
		out = append(out, list...)
	}
	return out
}

func weekendFromData(allMeetings []openf1.Meeting, allSessions []openf1.Session, now time.Time, allowPast bool) (*Weekend, error) {
	meetings := make(map[int]openf1.Meeting)
	for _, meeting := range allMeetings {
		if meeting.IsCancelled || !isRaceWeekend(meeting.MeetingName) {
			continue
		}
		meetings[meeting.MeetingKey] = meeting
	}

	type weekendCandidate struct {
		meetingKey int
		sessions   []parsedSession
	}

	byMeeting := make(map[int][]parsedSession)
	for _, session := range allSessions {
		if session.IsCancelled {
			continue
		}
		meeting, ok := meetings[session.MeetingKey]
		if !ok {
			continue
		}

		start, err := time.Parse(time.RFC3339, session.DateStart)
		if err != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339, session.DateEnd)
		if err != nil {
			continue
		}

		byMeeting[session.MeetingKey] = append(byMeeting[session.MeetingKey], parsedSession{
			session: session,
			meeting: meeting,
			start:   start,
			end:     end,
		})
	}

	var ongoing, upcoming, completed []weekendCandidate
	for meetingKey, sessions := range byMeeting {
		if len(sessions) == 0 {
			continue
		}

		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].start.Before(sessions[j].start)
		})

		first := sessions[0].start
		last := sessions[len(sessions)-1].end

		candidate := weekendCandidate{meetingKey: meetingKey, sessions: sessions}
		switch {
		case !now.Before(first) && now.Before(last):
			ongoing = append(ongoing, candidate)
		case now.Before(first):
			upcoming = append(upcoming, candidate)
		default:
			completed = append(completed, candidate)
		}
	}

	var chosen *weekendCandidate
	isOngoing := false

	if len(ongoing) > 0 {
		sort.Slice(ongoing, func(i, j int) bool {
			return ongoing[i].sessions[0].start.Before(ongoing[j].sessions[0].start)
		})
		chosen = &ongoing[0]
		isOngoing = true
	} else if len(upcoming) > 0 {
		sort.Slice(upcoming, func(i, j int) bool {
			return upcoming[i].sessions[0].start.Before(upcoming[j].sessions[0].start)
		})
		chosen = &upcoming[0]
	} else if allowPast && len(completed) > 0 {
		sort.Slice(completed, func(i, j int) bool {
			iLast := completed[i].sessions[len(completed[i].sessions)-1].end
			jLast := completed[j].sessions[len(completed[j].sessions)-1].end
			return iLast.After(jLast)
		})
		chosen = &completed[0]
	}

	if chosen == nil {
		return nil, fmt.Errorf("no upcoming F1 race weekends found")
	}

	first := chosen.sessions[0]
	weekend := &Weekend{
		MeetingName: first.meeting.MeetingName,
		Location:    first.meeting.Location,
		Country:     first.meeting.CountryName,
		Circuit:     first.session.CircuitShortName,
		GmtOffset:   first.session.GmtOffset,
		Ongoing:     isOngoing,
	}

	for _, s := range chosen.sessions {
		weekend.Sessions = append(weekend.Sessions, SessionEvent{
			Label:  sessionLabel(s.session.SessionName, s.session.SessionType),
			Status: sessionStatus(now, s.start, s.end),
			Start:  s.start,
			End:    s.end,
		})
	}

	return weekend, nil
}

type parsedSession struct {
	session openf1.Session
	meeting openf1.Meeting
	start   time.Time
	end     time.Time
}

func isRaceWeekend(name string) bool {
	return !strings.Contains(strings.ToLower(name), "testing")
}

func sessionStatus(now, start, end time.Time) SessionStatus {
	switch {
	case now.Before(start):
		return StatusUpcoming
	case !now.Before(end):
		return StatusCompleted
	default:
		return StatusOngoing
	}
}

func sessionLabel(name, sessionType string) string {
	if name == "" || strings.EqualFold(name, sessionType) {
		return sessionType
	}
	return fmt.Sprintf("%s (%s)", name, sessionType)
}

func (w *Weekend) NextUpcomingSession() *SessionEvent {
	for i := range w.Sessions {
		if w.Sessions[i].Status == StatusUpcoming {
			return &w.Sessions[i]
		}
	}
	return nil
}

func (w *Weekend) CurrentOngoingSession() *SessionEvent {
	for i := range w.Sessions {
		if w.Sessions[i].Status == StatusOngoing {
			return &w.Sessions[i]
		}
	}
	return nil
}

func (w *Weekend) Finished() bool {
	if len(w.Sessions) == 0 {
		return false
	}
	for _, session := range w.Sessions {
		if session.Status != StatusCompleted {
			return false
		}
	}
	return true
}

func (w *Weekend) CacheSavedDuringWeekend(loc *time.Location) bool {
	if !w.Cached || w.CacheSavedAt.IsZero() || len(w.Sessions) == 0 {
		return true
	}

	first := w.Sessions[0].Start
	last := w.Sessions[0].End
	for _, session := range w.Sessions[1:] {
		if session.Start.Before(first) {
			first = session.Start
		}
		if session.End.After(last) {
			last = session.End
		}
	}

	start := dayStart(first, loc)
	end := dayStart(last, loc).Add(24 * time.Hour)
	saved := w.CacheSavedAt.In(loc)
	return !saved.Before(start) && saved.Before(end)
}

func dayStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func StatusLabel(status SessionStatus) string {
	switch status {
	case StatusOngoing:
		return "ongoing"
	case StatusCompleted:
		return "completed"
	default:
		return "upcoming"
	}
}
