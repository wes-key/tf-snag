package ado

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	cases := []struct {
		in, org, project string
		wantErr          bool
	}{
		{in: "https://dev.azure.com/wes-key/tf-snag", org: "https://dev.azure.com/wes-key", project: "tf-snag"},
		{in: "https://dev.azure.com/wes-key/tf-snag/", org: "https://dev.azure.com/wes-key", project: "tf-snag"},
		{in: "https://wes-key.visualstudio.com/tf-snag", org: "https://wes-key.visualstudio.com", project: "tf-snag"},
		{in: "dev.azure.com/wes-key/tf-snag", wantErr: true}, // no scheme
		{in: "https://dev.azure.com/", wantErr: true},        // no project
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		org, project, err := ParseURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseURL(%q) = %q/%q, want an error", tc.in, org, project)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseURL(%q): %v", tc.in, err)
			continue
		}
		if org != tc.org || project != tc.project {
			t.Errorf("ParseURL(%q) = %q/%q, want %q/%q", tc.in, org, project, tc.org, tc.project)
		}
	}
}

// A PAT goes in Basic auth with an empty username; System.AccessToken is a JWT
// and must be a bearer. Sending the wrong one gets a sign-in page, not a 401.
func TestAuthHeaderScheme(t *testing.T) {
	pat := (&Client{Token: "abcdef0123456789abcdef0123456789"}).authHeader()
	if !strings.HasPrefix(pat, "Basic ") {
		t.Errorf("PAT auth = %q, want Basic", pat)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pat, "Basic "))
	if err != nil || !strings.HasPrefix(string(raw), ":") {
		t.Errorf("PAT should be sent as :<token>, got %q (%v)", raw, err)
	}

	jwt := (&Client{Token: "eyJhbGciOiJI.eyJzdWIiOiIx.SflKxwRJSM"}).authHeader()
	if !strings.HasPrefix(jwt, "Bearer ") {
		t.Errorf("System.AccessToken auth = %q, want Bearer", jwt)
	}
}

// stub is an Azure DevOps stand-in; handlers are keyed by URL path suffix.
func stub(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for suffix, h := range routes {
			if strings.Contains(r.URL.Path, suffix) {
				h(w, r)
				return
			}
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "no route", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return &Client{OrgURL: srv.URL, Project: "proj", Token: "pat", HTTP: srv.Client()}
}

func TestExistingMapsFindingIDs(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"wit/wiql": func(w http.ResponseWriter, r *http.Request) {
			var q map[string]string
			json.NewDecoder(r.Body).Decode(&q)
			if !strings.Contains(q["query"], MarkerTag) {
				t.Errorf("WIQL does not filter on the marker tag: %q", q["query"])
			}
			io.WriteString(w, `{"workItems":[{"id":11},{"id":12},{"id":13}]}`)
		},
		"wit/workitemsbatch": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"value":[
              {"id":14,"fields":{"System.State":"Active","System.Title":"drift b again","System.Tags":"tf-snag; tf-snag-id-bbb"}},
              {"id":11,"fields":{"System.State":"Active","System.Title":"drift a","System.Tags":"tf-snag; tf-snag-id-AAA; drift"}},
              {"id":12,"fields":{"System.State":"Closed","System.Title":"drift b","System.Tags":"tf-snag; tf-snag-id-bbb"}},
              {"id":13,"fields":{"System.State":"New","System.Title":"hand made","System.Tags":"tf-snag"}}
            ]}`)
		},
	})

	got, err := c.Existing()
	if err != nil {
		t.Fatal(err)
	}
	// Azure DevOps normalises tag case, so lookups must be case-insensitive:
	// "tf-snag-id-AAA" has to come back under "aaa".
	if items := got["aaa"]; len(items) != 1 || items[0].ID != 11 || items[0].State != "Active" {
		t.Errorf("id aaa = %+v, want just work item 11 Active", items)
	}
	// Every item for a finding comes back, oldest first — including closed ones.
	// Collapsing to one was the duplicate bug: a closed original hid the open
	// item that had superseded it, so every run raised another.
	items := got["bbb"]
	if len(items) != 2 {
		t.Fatalf("id bbb = %+v, want both items", items)
	}
	if items[0].ID != 12 || items[1].ID != 14 {
		t.Errorf("id bbb ordered %d, %d — want oldest first", items[0].ID, items[1].ID)
	}
	// An item carrying only the marker tag has no finding id and is ignored
	// rather than erroring — someone may have added the tag by hand.
	if len(got) != 2 {
		t.Errorf("Existing() = %d findings, want 2 (the untagged item skipped)", len(got))
	}
	if u := got["aaa"][0].URL; u == "" || !strings.Contains(u, "_workitems/edit/11") {
		t.Errorf("web URL = %q, want a browser link", u)
	}
}

func TestExistingEmptyWhenNothingTagged(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"wit/wiql": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"workItems":[]}`)
		},
		// No batch call should happen with nothing to fetch.
	})
	got, err := c.Existing()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d items, want none", len(got))
	}
}

func TestCreateSendsPatchDocument(t *testing.T) {
	var gotPath, gotType string
	var patches []patch
	c := stub(t, map[string]http.HandlerFunc{
		"wit/workitems/": func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
			json.NewDecoder(r.Body).Decode(&patches)
			io.WriteString(w, `{"id":42,"fields":{"System.State":"New","System.Title":"t"}}`)
		},
	})

	item, err := c.Create(NewItem{
		FindingID: "abc123", Type: "Bug", Title: "t",
		Description: "<p>body</p>", Tags: []string{"drift"}, AreaPath: "proj\\Platform",
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != 42 || !strings.Contains(item.URL, "edit/42") {
		t.Errorf("created item = %+v", item)
	}
	// The work item type goes in the path as $Type.
	if !strings.Contains(gotPath, "$Bug") {
		t.Errorf("path = %q, want the $Bug type segment", gotPath)
	}
	// Sending application/json instead gets a 400 that does not explain itself.
	if gotType != "application/json-patch+json" {
		t.Errorf("content-type = %q, want application/json-patch+json", gotType)
	}

	fields := map[string]any{}
	for _, p := range patches {
		if p.Op != "add" {
			t.Errorf("patch op = %q, want add", p.Op)
		}
		fields[p.Path] = p.Value
	}
	if fields["/fields/System.Title"] != "t" {
		t.Errorf("title not set: %+v", fields)
	}
	if fields["/fields/System.AreaPath"] != "proj\\Platform" {
		t.Errorf("area path not set: %+v", fields)
	}
	// Both tags must be present or the next run cannot find this item again.
	tags, _ := fields["/fields/System.Tags"].(string)
	for _, want := range []string{MarkerTag, IDTagPrefix + "abc123", "drift"} {
		if !strings.Contains(tags, want) {
			t.Errorf("tags %q missing %q", tags, want)
		}
	}
}

func TestValidateUsesValidateOnly(t *testing.T) {
	var gotQuery string
	c := stub(t, map[string]http.HandlerFunc{
		"wit/workitems/": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			io.WriteString(w, `{"id":0,"fields":{}}`)
		},
	})
	if err := c.Validate(NewItem{FindingID: "x", Type: "Task", Title: "check"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "validateOnly=true") {
		t.Errorf("query = %q, want validateOnly=true so the check leaves nothing behind", gotQuery)
	}
}

// Azure DevOps answers an unauthenticated API call with 203 and a sign-in page
// rather than 401, which is otherwise very hard to diagnose.
func TestUnauthenticatedIsReportedClearly(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"wit/workitems/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNonAuthoritativeInfo)
			io.WriteString(w, "<html>sign in</html>")
		},
	})
	err := c.Validate(NewItem{Type: "Task", Title: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error should name the cause, got: %v", err)
	}
}

func TestForbiddenNamesThePermission(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"wit/workitems/": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Access denied."}`, http.StatusForbidden)
		},
	})
	err := c.Validate(NewItem{Type: "Task", Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "Work Items (Read & Write)") {
		t.Errorf("error should say which permission is needed, got: %v", err)
	}
}

func TestCloseSkipsItemAlreadyInState(t *testing.T) {
	var calls int
	c := stub(t, map[string]http.HandlerFunc{
		"wit/workitems/": func(w http.ResponseWriter, r *http.Request) {
			calls++
			io.WriteString(w, `{"id":7,"fields":{}}`)
		},
	})

	changed, err := c.Close(WorkItem{ID: 7, State: "Closed"}, "Closed", "because")
	if err != nil {
		t.Fatal(err)
	}
	if changed || calls != 0 {
		t.Errorf("changed=%v calls=%d, want no request for an item already Closed", changed, calls)
	}

	changed, err = c.Close(WorkItem{ID: 7, State: "Active"}, "Closed", "because")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || calls != 1 {
		t.Errorf("changed=%v calls=%d, want one PATCH", changed, calls)
	}
}

func TestSplitAndFindTags(t *testing.T) {
	tags := splitTags(" tf-snag;  tf-snag-id-Deadbeef ; drift ")
	if len(tags) != 3 || tags[1] != "tf-snag-id-Deadbeef" {
		t.Fatalf("splitTags = %q", tags)
	}
	if got := findingIDFromTags(tags); got != "deadbeef" {
		t.Errorf("findingIDFromTags = %q, want the lowercased id", got)
	}
	if got := findingIDFromTags([]string{"tf-snag", "drift"}); got != "" {
		t.Errorf("findingIDFromTags with no id tag = %q, want empty", got)
	}
}

// The closed state is a process-template concern: Agile ends at "Closed", Scrum
// and Basic at "Done". Getting it wrong produces a 400 only when a finding
// disappears, so it is resolved and checked up front instead.
func TestResolveClosedState(t *testing.T) {
	basic := []State{
		{Name: "To Do", Category: "Proposed"},
		{Name: "Doing", Category: "InProgress"},
		{Name: "Done", Category: "Completed"},
	}
	newStates := func(s []State) *Client {
		return stub(t, map[string]http.HandlerFunc{
			"workitemtypes": func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"value": s})
			},
		})
	}

	// No preference: take the type's own completed state.
	got, err := newStates(basic).ResolveClosedState("Task", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Done" {
		t.Errorf("resolved %q, want Done from the Completed category", got)
	}

	// A supplied state that exists is honoured, and its canonical casing used.
	if got, err := newStates(basic).ResolveClosedState("Task", "done"); err != nil || got != "Done" {
		t.Errorf("ResolveClosedState(done) = %q, %v — want the canonical Done", got, err)
	}

	// A supplied state that does not exist fails now, naming what does. This is
	// the case that used to surface as an opaque 400 mid-run.
	_, err = newStates(basic).ResolveClosedState("Task", "Closed")
	if err == nil {
		t.Fatal("expected an error for a state the template does not have")
	}
	for _, want := range []string{"Closed", "To Do", "Doing", "Done"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name the offending state and the valid ones, missing %q: %v", want, err)
		}
	}

	// A type with no completed state cannot be closed into automatically.
	_, err = newStates([]State{{Name: "New", Category: "Proposed"}}).ResolveClosedState("Task", "")
	if err == nil || !strings.Contains(err.Error(), "-ado-closed-state") {
		t.Errorf("error should point at the flag: %v", err)
	}
}
