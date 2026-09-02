package teams

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testClient is a Client that never really sleeps, so the retry paths run fast.
func testClient(slept *[]time.Duration) *Client {
	return &Client{Sleep: func(d time.Duration) { *slept = append(*slept, d) }}
}

func TestPostSuccess(t *testing.T) {
	var gotBody, gotType, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotType, gotMethod = string(b), r.Header.Get("Content-Type"), r.Method
		w.WriteHeader(http.StatusAccepted) // what a Workflows trigger returns
	}))
	defer srv.Close()

	var slept []time.Duration
	if err := testClient(&slept).Post(srv.URL, []byte(`{"type":"message"}`)); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s", gotMethod)
	}
	if gotBody != `{"type":"message"}` {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.HasPrefix(gotType, "application/json") {
		t.Errorf("content-type = %q", gotType)
	}
	if len(slept) != 0 {
		t.Errorf("slept %v on a first-try success", slept)
	}
}

func TestPostRetriesServerErrorThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "upstream wobbled", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var slept []time.Duration
	if err := testClient(&slept).Post(srv.URL, []byte("{}")); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(slept) != 1 || slept[0] != time.Second {
		t.Errorf("backoff = %v, want [1s]", slept)
	}
}

func TestPostGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "still down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var slept []time.Duration
	err := testClient(&slept).Post(srv.URL, []byte("{}"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if int(calls) != maxAttempts {
		t.Errorf("calls = %d, want %d", calls, maxAttempts)
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "still down") {
		t.Errorf("error should carry the status and body: %v", err)
	}
	// 1s then 2s.
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Errorf("backoff = %v, want [1s 2s]", slept)
	}
}

// A 4xx means the payload or the URL is wrong; retrying cannot fix it.
func TestPostDoesNotRetryClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "flow not found", http.StatusNotFound)
	}))
	defer srv.Close()

	var slept []time.Duration
	err := testClient(&slept).Post(srv.URL, []byte("{}"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", calls)
	}
	if len(slept) != 0 {
		t.Errorf("slept %v on a non-retryable failure", slept)
	}
}

func TestPostHonoursRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "7")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var slept []time.Duration
	if err := testClient(&slept).Post(srv.URL, []byte("{}")); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("backoff = %v, want [7s] from Retry-After", slept)
	}
}

// The webhook URL is a credential — it must not surface in an error, which CI
// will happily print to a log.
func TestPostRedactsWebhookURL(t *testing.T) {
	// A port nothing is listening on: net/http puts the whole URL in the error.
	url := "http://127.0.0.1:1/secret-flow-token"
	c := &Client{Sleep: func(time.Duration) {}, Timeout: 200 * time.Millisecond}
	err := c.Post(url, []byte("{}"))
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "secret-flow-token") {
		t.Errorf("error leaked the webhook URL: %v", err)
	}
	if !strings.Contains(err.Error(), "<webhook>") {
		t.Errorf("error should name the redaction: %v", err)
	}
}

func TestPostRejectsEmptyURL(t *testing.T) {
	if err := (&Client{}).Post("   ", []byte("{}")); err == nil {
		t.Fatal("expected an error for an empty webhook URL")
	}
}
