// Package teams posts a prepared Adaptive Card payload to a Microsoft Teams
// webhook — a Power Automate "Workflows" HTTP trigger (the retired Office 365
// connector endpoints are not supported).
//
// It is deliberately the only place in tf-snag that talks to the network: the
// card itself is built by internal/report, which stays a pure transformer.
package teams

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a single attempt. Workflows triggers normally answer in
// well under a second; anything slower is a problem we would rather surface
// than let a scheduled job hang on.
const DefaultTimeout = 15 * time.Second

// maxAttempts is the total number of tries, so at most two retries.
const maxAttempts = 3

// Client posts to a Teams webhook. The zero value is usable and posts with
// DefaultTimeout via http.DefaultTransport.
type Client struct {
	HTTP    *http.Client
	Timeout time.Duration
	// Sleep is called between retries; a var so tests need not actually wait.
	Sleep func(time.Duration)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	t := c.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	return &http.Client{Timeout: t}
}

func (c *Client) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Post sends body to url. A Workflows trigger answers 200/202 with an empty
// body; 429 and 5xx are retried (honouring Retry-After), 4xx is returned
// immediately since retrying a rejected payload or a dead URL cannot help.
//
// The URL is a secret: it is never included in an error message, so a failure
// can be logged in CI without leaking the webhook.
func (c *Client) Post(url string, body []byte) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("teams: empty webhook URL")
	}
	client := c.httpClient()

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			c.sleep(backoff(attempt, last))
		}
		retryable, err := c.attempt(client, url, body)
		if err == nil {
			return nil
		}
		last = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("teams: giving up after %d attempts: %w", maxAttempts, last)
}

// attempt makes one request, reporting whether the failure is worth retrying.
func (c *Client) attempt(client *http.Client, url string, body []byte) (retryable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("teams: building request: %w", redact(err, url))
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		// Connection reset, DNS blip, timeout: worth another go.
		return true, fmt.Errorf("teams: posting card: %w", redact(err, url))
	}
	defer resp.Body.Close()

	// Drain so the connection can be reused, and keep a little of it for the
	// error message — Workflows explains rejections in the body.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	err = fmt.Errorf("teams: webhook returned %s%s", resp.Status, detail(snippet))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return true, retryAfterErr{err: err, after: retryAfter(resp)}
	}
	return false, err
}

func detail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	return ": " + strings.Join(strings.Fields(s), " ")
}

// retryAfterErr carries a server-requested delay through to the backoff.
type retryAfterErr struct {
	err   error
	after time.Duration
}

func (e retryAfterErr) Error() string { return e.err.Error() }
func (e retryAfterErr) Unwrap() error { return e.err }

func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	// Only the delta-seconds form; an HTTP-date is rare here and the default
	// backoff is a fine fallback.
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// backoff is 1s then 2s, unless the server asked for longer.
func backoff(attempt int, last error) time.Duration {
	d := time.Duration(1<<(attempt-2)) * time.Second
	var ra retryAfterErr
	if errors.As(last, &ra) && ra.after > d {
		return ra.after
	}
	return d
}

// redact strips the webhook URL out of an error. net/http puts the full URL in
// *url.Error, and that URL is the credential.
func redact(err error, url string) error {
	if err == nil || url == "" {
		return err
	}
	if s := err.Error(); strings.Contains(s, url) {
		return fmt.Errorf("%s", strings.ReplaceAll(s, url, "<webhook>"))
	}
	return err
}
