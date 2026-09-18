package ado

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// prServer fakes the Azure DevOps pull request threads API and records what it
// was sent, so the tests assert on the requests rather than on a live project.
type prServer struct {
	threads []PRThread
	status  int // non-zero replies with this instead of the threads

	requests []prRequest
}

type prRequest struct {
	method string
	path   string
	body   map[string]any
}

func newPRServer(t *testing.T, s *prServer) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := prRequest{method: r.Method, path: r.URL.Path}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			json.Unmarshal(raw, &rec.body)
		}
		s.requests = append(s.requests, rec)

		if s.status != 0 {
			w.WriteHeader(s.status)
			w.Write([]byte(`{"message":"nope"}`))
			return
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"value": s.threads})
		case http.MethodPost:
			json.NewEncoder(w).Encode(PRThread{ID: 42, Status: ThreadActive})
		default: // PATCH
			json.NewEncoder(w).Encode(PRThread{ID: 42})
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{OrgURL: srv.URL, Project: "tf-snag", Token: "pat"}
}

func TestPRThreads(t *testing.T) {
	s := &prServer{threads: []PRThread{{
		ID:       3,
		Status:   ThreadActive,
		Comments: []PRComment{{ID: 30, Content: "hello"}},
	}}}
	c := newPRServer(t, s)

	got, err := c.PRThreads("repo-1", 7)
	if err != nil {
		t.Fatalf("PRThreads: %v", err)
	}
	if len(got) != 1 || got[0].ID != 3 || got[0].Comments[0].Content != "hello" {
		t.Fatalf("PRThreads returned %+v", got)
	}
	// The repository and pull request have to reach the URL, not the body.
	if want := "/tf-snag/_apis/git/repositories/repo-1/pullRequests/7/threads"; s.requests[0].path != want {
		t.Errorf("path = %q, want %q", s.requests[0].path, want)
	}
}

func TestCreatePRThread(t *testing.T) {
	s := &prServer{}
	c := newPRServer(t, s)

	got, err := c.CreatePRThread("repo-1", 7, "the comment", ThreadActive)
	if err != nil {
		t.Fatalf("CreatePRThread: %v", err)
	}
	if got.ID != 42 {
		t.Errorf("thread id = %d, want 42", got.ID)
	}

	req := s.requests[0]
	if req.method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.method)
	}
	comments, _ := req.body["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("body carried %d comments, want 1: %+v", len(comments), req.body)
	}
	first, _ := comments[0].(map[string]any)
	if first["content"] != "the comment" {
		t.Errorf("content = %v, want the comment", first["content"])
	}
	// commentType "text" is what makes it a normal comment rather than a code
	// review vote or a system entry.
	if first["commentType"] != "text" {
		t.Errorf("commentType = %v, want text", first["commentType"])
	}
	if req.body["status"] != ThreadActive {
		t.Errorf("status = %v, want %q", req.body["status"], ThreadActive)
	}
}

func TestUpdatePRComment(t *testing.T) {
	s := &prServer{}
	c := newPRServer(t, s)

	if err := c.UpdatePRComment("repo-1", 7, 3, 30, "edited"); err != nil {
		t.Fatalf("UpdatePRComment: %v", err)
	}
	req := s.requests[0]
	if req.method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", req.method)
	}
	if want := "/threads/3/comments/30"; !strings.HasSuffix(req.path, want) {
		t.Errorf("path = %q, want it to end %q", req.path, want)
	}
	if req.body["content"] != "edited" {
		t.Errorf("content = %v, want edited", req.body["content"])
	}
}

func TestSetPRThreadStatus(t *testing.T) {
	s := &prServer{}
	c := newPRServer(t, s)

	if err := c.SetPRThreadStatus("repo-1", 7, 3, ThreadClosed); err != nil {
		t.Fatalf("SetPRThreadStatus: %v", err)
	}
	req := s.requests[0]
	if req.method != http.MethodPatch || !strings.HasSuffix(req.path, "/threads/3") {
		t.Errorf("request = %s %s, want PATCH .../threads/3", req.method, req.path)
	}
	if req.body["status"] != ThreadClosed {
		t.Errorf("status = %v, want %q", req.body["status"], ThreadClosed)
	}
}

// The permission is the thing people get wrong, so a 403 has to name it rather
// than leaving somebody guessing which of the repository scopes is missing.
func TestPRThreadsNamesThePermissionOn403(t *testing.T) {
	c := newPRServer(t, &prServer{status: http.StatusForbidden})

	_, err := c.PRThreads("repo-1", 7)
	if err == nil {
		t.Fatal("PRThreads succeeded on a 403")
	}
	if !strings.Contains(err.Error(), PRPermission) {
		t.Errorf("error = %v, want it to name %q", err, PRPermission)
	}
}

// Azure DevOps hides what an identity cannot see, so this arrives as a 404 and
// the hint has to cover both possibilities.
func TestPRThreadsExplainsA404(t *testing.T) {
	c := newPRServer(t, &prServer{status: http.StatusNotFound})

	_, err := c.PRThreads("repo-1", 7)
	if err == nil {
		t.Fatal("PRThreads succeeded on a 404")
	}
	for _, want := range []string{"repository and pull request exist", "404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}
