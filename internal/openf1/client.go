package openf1

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.openf1.org/v1"

const liveRestrictionMarker = "Live F1 session in progress"

const (
	defaultRequestInterval = 400 * time.Millisecond
	maxRateLimitAttempts   = 3
	defaultRetryDelay      = time.Second
	maxRetryDelay          = 5 * time.Second
)

type Session struct {
	SessionKey       int    `json:"session_key"`
	SessionType      string `json:"session_type"`
	SessionName      string `json:"session_name"`
	DateStart        string `json:"date_start"`
	DateEnd          string `json:"date_end"`
	MeetingKey       int    `json:"meeting_key"`
	CircuitShortName string `json:"circuit_short_name"`
	Location         string `json:"location"`
	CountryName      string `json:"country_name"`
	GmtOffset        string `json:"gmt_offset"`
	IsCancelled      bool   `json:"is_cancelled"`
}

type Meeting struct {
	MeetingKey  int    `json:"meeting_key"`
	MeetingName string `json:"meeting_name"`
	Location    string `json:"location"`
	CountryName string `json:"country_name"`
	IsCancelled bool   `json:"is_cancelled"`
}

type Client struct {
	http    *http.Client
	baseURL string

	mu       sync.Mutex
	next     time.Time
	interval time.Duration
	sleep    func(time.Duration)
	clock    func() time.Time
}

type APIError struct {
	URL        string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("fetch %s: status %d: %s", e.URL, e.StatusCode, e.Body)
}

func IsLiveRestriction(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		return false
	}
	return strings.Contains(apiErr.Body, liveRestrictionMarker)
}

func IsRateLimited(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusTooManyRequests
}

func NewClient() *Client {
	return &Client{
		http:     &http.Client{Timeout: 15 * time.Second},
		baseURL:  defaultBaseURL,
		interval: defaultRequestInterval,
		sleep:    time.Sleep,
		clock:    time.Now,
	}
}

func (c *Client) Sessions(year int) ([]Session, error) {
	var sessions []Session
	if err := c.get(fmt.Sprintf("%s/sessions?year=%d", c.baseURL, year), &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (c *Client) Meetings(year int) ([]Meeting, error) {
	var meetings []Meeting
	if err := c.get(fmt.Sprintf("%s/meetings?year=%d", c.baseURL, year), &meetings); err != nil {
		return nil, err
	}
	return meetings, nil
}

func (c *Client) get(url string, dest any) error {
	var lastErr error
	for attempt := 1; attempt <= maxRateLimitAttempts; attempt++ {
		c.waitForTurn()

		resp, err := c.http.Get(url)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", url, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			lastErr = &APIError{URL: url, StatusCode: resp.StatusCode, Body: string(body)}
			if attempt == maxRateLimitAttempts {
				break
			}
			c.sleepFor(c.retryDelay(resp))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return &APIError{URL: url, StatusCode: resp.StatusCode, Body: string(body)}
		}

		err = json.NewDecoder(resp.Body).Decode(dest)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode response from %s: %w", url, err)
		}
		return nil
	}
	return lastErr
}

// waitForTurn spaces request starts out so the client stays under OpenF1's
// limit of 3 requests per second.
func (c *Client) waitForTurn() {
	interval := c.requestInterval()
	if interval <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.nowTime()
	if delay := c.next.Sub(now); delay > 0 {
		c.sleepFor(delay)
		now = c.nowTime()
	}
	c.next = now.Add(interval)
}

func (c *Client) requestInterval() time.Duration {
	switch {
	case c.interval < 0:
		return 0
	case c.interval == 0:
		return defaultRequestInterval
	default:
		return c.interval
	}
}

func (c *Client) retryDelay(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return defaultRetryDelay
	}

	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return capRetryDelay(time.Duration(seconds) * time.Second)
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := when.Sub(c.nowTime())
		if delay < 0 {
			delay = 0
		}
		return capRetryDelay(delay)
	}
	return defaultRetryDelay
}

func capRetryDelay(delay time.Duration) time.Duration {
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func (c *Client) nowTime() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

func (c *Client) sleepFor(delay time.Duration) {
	if delay <= 0 {
		return
	}
	if c.sleep != nil {
		c.sleep(delay)
		return
	}
	time.Sleep(delay)
}
