// Package ado raises and closes Azure DevOps work items for tf-snag findings.
//
// A finding's identity is its stable FindingID (the same guid the SARIF result
// carries). Every work item tf-snag creates is tagged `tf-snag:<id>`, and each
// run queries those tags back out — so Azure DevOps, not a build artifact, is
// the record of what has already been raised. That matters: artifact retention
// expires, pipelines get rebuilt, and a lost baseline must not turn into a
// second work item for drift that is already being tracked.
package ado

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// apiVersion is pinned: Azure DevOps changes payload shapes between versions,
// and an unpinned request follows whatever the server defaults to.
const apiVersion = "7.0"

// DefaultTimeout bounds a single request.
const DefaultTimeout = 30 * time.Second

// MarkerTag is on every work item tf-snag creates, so one query finds them all
// regardless of how many findings this run has.
const MarkerTag = "tf-snag"

// IgnoredTag marks an item whose finding is currently covered by an ignore rule.
// It is what stops the note below being posted again on every subsequent run —
// a daily "still ignored" comment for months would be worse than useless.
const IgnoredTag = "tf-snag-ignored"

// IDTagPrefix prefixes the per-finding tag, e.g. "tf-snag-id:9c1f…".
//
// A colon would be neater but Azure DevOps splits tags on some punctuation and
// normalises case, so the prefix is hyphenated and matching is case-insensitive.
const IDTagPrefix = "tf-snag-id-"

// Client talks to one Azure DevOps project.
type Client struct {
	// OrgURL is the collection root, e.g. https://dev.azure.com/wes-key
	OrgURL string
	// Project is the project name or id.
	Project string
	// Token is a PAT or an OAuth bearer (System.AccessToken). Which scheme to
	// use is detected — see authHeader.
	Token string

	HTTP    *http.Client
	Timeout time.Duration
}

// WorkItem is the subset of a work item tf-snag reasons about.
type WorkItem struct {
	ID    int
	State string
	Title string
	Tags  []string
	URL   string // human URL, not the API one
}

// ParseURL splits a project URL — https://dev.azure.com/org/project — into the
// org root and project name. Accepts a trailing slash and the older
// org.visualstudio.com form.
func ParseURL(raw string) (orgURL, project string, err error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil {
		return "", "", fmt.Errorf("ado: parsing %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("ado: %q is not an absolute URL (want https://dev.azure.com/<org>/<project>)", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return "", "", fmt.Errorf("ado: %q has no project segment (want https://dev.azure.com/<org>/<project>)", raw)
	}
	project = parts[len(parts)-1]
	u.Path = "/" + strings.Join(parts[:len(parts)-1], "/")
	return strings.TrimRight(u.String(), "/"), project, nil
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

// authHeader picks the scheme from the token's shape. A PAT is an opaque string
// and goes in Basic auth with an empty username; System.AccessToken is a JWT
// (three dot-separated segments) and must be sent as a bearer. Guessing wrong
// yields a 203 with a sign-in page rather than a clean 401, so it is worth
// getting right without asking the caller to declare it.
func (c *Client) authHeader() string {
	if t := strings.TrimSpace(c.Token); strings.Count(t, ".") == 2 && strings.HasPrefix(t, "ey") {
		return "Bearer " + t
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+c.Token))
}

// do issues one request and decodes a JSON response into out (which may be nil).
func (c *Client) do(method, endpoint string, body any, contentType string, out any) error {
	_, err := c.doWith(reqOpts{
		method: method, endpoint: endpoint, body: body, contentType: contentType, out: out,
	})
	return err
}

// reqOpts is the full shape of a request. do covers the work item calls; the
// wiki needs request headers (If-Match), the response headers (the ETag it will
// send back next time) and a 404 that is an answer rather than a failure.
type reqOpts struct {
	method      string
	endpoint    string
	body        any
	contentType string
	out         any
	headers     map[string]string
	// needs names the permission a 403 is complaining about. Defaults to the
	// work item scope, which is what nearly every call here wants.
	needs string
	// allow404 returns the status instead of an error, for "does this exist?".
	allow404 bool
}

func (c *Client) doWith(o reqOpts) (*http.Response, error) {
	var rdr io.Reader
	if o.body != nil {
		raw, err := json.Marshal(o.body)
		if err != nil {
			return nil, fmt.Errorf("ado: encoding request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(o.method, o.endpoint, rdr)
	if err != nil {
		return nil, fmt.Errorf("ado: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", c.authHeader())
	if o.contentType != "" {
		req.Header.Set("Content-Type", o.contentType)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ado: %s: %w", describe(o.method, o.endpoint), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if o.allow404 && resp.StatusCode == http.StatusNotFound {
		return resp, nil
	}
	if err := checkStatus(resp, raw, o.method, o.endpoint, o.needs); err != nil {
		return resp, err
	}
	if o.out == nil {
		return resp, nil
	}
	if err := json.Unmarshal(raw, o.out); err != nil {
		return resp, fmt.Errorf("ado: decoding %s response: %w", describe(o.method, o.endpoint), err)
	}
	return resp, nil
}

// checkStatus turns a non-2xx into an error that says what to do about it.
// Azure DevOps answers an unauthenticated API call with 203 and an HTML sign-in
// page rather than 401, which is otherwise a baffling failure to debug.
func checkStatus(resp *http.Response, body []byte, method, endpoint, needs string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.StatusCode != http.StatusNonAuthoritativeInfo {
		return nil
	}
	if needs == "" {
		needs = "Work Items (Read & Write)"
	}
	switch resp.StatusCode {
	case http.StatusNonAuthoritativeInfo, http.StatusUnauthorized:
		return fmt.Errorf("ado: not authenticated (%s) — check the token is valid and not expired", resp.Status)
	case http.StatusForbidden:
		return fmt.Errorf("ado: token lacks permission for %s (%s) — it needs %s",
			describe(method, endpoint), resp.Status, needs)
	case http.StatusNotFound:
		return fmt.Errorf("ado: %s returned %s — check the organisation, project and work item type exist",
			describe(method, endpoint), resp.Status)
	}
	return fmt.Errorf("ado: %s returned %s%s", describe(method, endpoint), resp.Status, detail(body))
}

// describe names the call without leaking the query string, which can carry a
// WIQL fragment.
func describe(method, endpoint string) string {
	if i := strings.Index(endpoint, "?"); i >= 0 {
		endpoint = endpoint[:i]
	}
	return method + " " + endpoint
}

// detail appends the API's own message when it sent one.
func detail(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return ": " + strings.Join(strings.Fields(e.Message), " ")
	}
	if s := strings.TrimSpace(string(body)); s != "" && !strings.HasPrefix(s, "<") {
		return ": " + strings.Join(strings.Fields(s), " ")
	}
	return ""
}

func (c *Client) projectURL(path string) string {
	return fmt.Sprintf("%s/%s/_apis/%s", c.OrgURL, url.PathEscape(c.Project), path)
}

// --- reading -----------------------------------------------------------------

// Existing returns the work items tf-snag has already raised, grouped by finding
// id and ordered oldest first. Two calls regardless of how many findings this
// run has: one WIQL query for the marker tag, then one batch fetch for the
// fields.
//
// Items whose tags no longer parse into an id are skipped rather than erroring —
// someone editing tags by hand should not break the run.
func (c *Client) Existing() (map[string][]WorkItem, error) {
	ids, err := c.queryMarked()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return map[string][]WorkItem{}, nil
	}
	items, err := c.batchGet(ids)
	if err != nil {
		return nil, err
	}

	// Every item for a finding, oldest first — not just one. Collapsing to a
	// single item here was a bug: it kept the lowest id, so once an original had
	// been closed the caller never saw the open item that superseded it and
	// raised another duplicate on every run.
	out := map[string][]WorkItem{}
	for _, it := range items {
		if id := findingIDFromTags(it.Tags); id != "" {
			out[id] = append(out[id], it)
		}
	}
	for id := range out {
		sort.Slice(out[id], func(i, j int) bool { return out[id][i].ID < out[id][j].ID })
	}
	return out, nil
}

// StateCategories maps each of a work item type's states to its process-template
// category, so "is this finished" does not depend on recognising a state name.
func (c *Client) StateCategories(itemType string) (map[string]string, error) {
	states, err := c.States(itemType)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(states))
	for _, s := range states {
		out[strings.ToLower(s.Name)] = s.Category
	}
	return out, nil
}

// queryMarked runs the WIQL query for every work item carrying the marker tag.
func (c *Client) queryMarked() ([]int, error) {
	// Scoped to the project by the URL. Closed items are included on purpose:
	// a finding that comes back should reopen the conversation on the original
	// item rather than spawn a second one.
	q := map[string]string{
		"query": fmt.Sprintf(
			"SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project AND [System.Tags] CONTAINS '%s'",
			MarkerTag),
	}
	var res struct {
		WorkItems []struct {
			ID int `json:"id"`
		} `json:"workItems"`
	}
	if err := c.do(http.MethodPost, c.projectURL("wit/wiql")+"?api-version="+apiVersion,
		q, "application/json", &res); err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(res.WorkItems))
	for _, w := range res.WorkItems {
		ids = append(ids, w.ID)
	}
	return ids, nil
}

// batchGetLimit is the maximum ids the workitemsbatch endpoint accepts.
const batchGetLimit = 200

func (c *Client) batchGet(ids []int) ([]WorkItem, error) {
	var out []WorkItem
	for start := 0; start < len(ids); start += batchGetLimit {
		end := start + batchGetLimit
		if end > len(ids) {
			end = len(ids)
		}
		body := map[string]any{
			"ids":    ids[start:end],
			"fields": []string{"System.Id", "System.State", "System.Title", "System.Tags"},
		}
		var res struct {
			Value []struct {
				ID     int `json:"id"`
				Fields struct {
					State string `json:"System.State"`
					Title string `json:"System.Title"`
					Tags  string `json:"System.Tags"`
				} `json:"fields"`
			} `json:"value"`
		}
		if err := c.do(http.MethodPost, c.projectURL("wit/workitemsbatch")+"?api-version="+apiVersion,
			body, "application/json", &res); err != nil {
			return nil, err
		}
		for _, w := range res.Value {
			out = append(out, WorkItem{
				ID:    w.ID,
				State: w.Fields.State,
				Title: w.Fields.Title,
				Tags:  splitTags(w.Fields.Tags),
				URL:   c.WebURL(w.ID),
			})
		}
	}
	return out, nil
}

// WebURL is the browser URL for a work item — what goes in a report, as opposed
// to the _apis one the REST calls use.
func (c *Client) WebURL(id int) string {
	return fmt.Sprintf("%s/%s/_workitems/edit/%d", c.OrgURL, url.PathEscape(c.Project), id)
}

// splitTags parses the "a; b; c" form the API returns.
func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// findingIDFromTags pulls the finding id back out of a work item's tags. Azure
// DevOps normalises tag case, so the comparison is case-insensitive and the id
// is returned lowercased to match how it was written.
func findingIDFromTags(tags []string) string {
	for _, t := range tags {
		if len(t) > len(IDTagPrefix) && strings.EqualFold(t[:len(IDTagPrefix)], IDTagPrefix) {
			return strings.ToLower(t[len(IDTagPrefix):])
		}
	}
	return ""
}

// State is one state a work item type can be in, and the category the process
// template files it under. Categories are stable across templates even though
// the names are not: Agile ends at "Closed", Scrum and Basic at "Done".
type State struct {
	Name     string `json:"name"`
	Category string `json:"category"` // Proposed, InProgress, Resolved, Completed, Removed
}

// States lists the states a work item type supports. Used to resolve or check
// the closed state before anything is closed, because the alternative is a 400
// halfway through a run that says only that the value is unsupported.
func (c *Client) States(itemType string) ([]State, error) {
	endpoint := fmt.Sprintf("%s/%s/_apis/wit/workitemtypes/%s/states?api-version=%s",
		c.OrgURL, url.PathEscape(c.Project), url.PathEscape(itemType), apiVersion)
	var res struct {
		Value []State `json:"value"`
	}
	if err := c.do(http.MethodGet, endpoint, nil, "", &res); err != nil {
		return nil, err
	}
	return res.Value, nil
}

// ResolveClosedState decides which state a resolved finding's item moves to.
//
// An empty want picks the type's own completed state, so the common templates
// work with no configuration: Agile gets "Closed", Scrum and Basic "Done". A
// supplied value is checked against the list and, if wrong, the error names what
// is actually available — the raw API failure says only that the value is not
// supported, without saying what is.
func (c *Client) ResolveClosedState(itemType, want string) (string, error) {
	states, err := c.States(itemType)
	if err != nil {
		return "", err
	}
	if len(states) == 0 {
		if want == "" {
			return "", fmt.Errorf("ado: %q has no states to close into", itemType)
		}
		return want, nil // nothing to check against; let the PATCH decide
	}

	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, s.Name)
		if want != "" && strings.EqualFold(s.Name, want) {
			return s.Name, nil
		}
	}
	if want != "" {
		return "", fmt.Errorf("ado: %q is not a state of %q — this project's are: %s",
			want, itemType, strings.Join(names, ", "))
	}
	for _, s := range states {
		if strings.EqualFold(s.Category, "Completed") {
			return s.Name, nil
		}
	}
	return "", fmt.Errorf("ado: %q has no completed state to close into (states: %s); pass -ado-closed-state",
		itemType, strings.Join(names, ", "))
}

// --- writing -----------------------------------------------------------------

// NewItem is the work item to raise for a finding.
type NewItem struct {
	FindingID   string
	Type        string // Task, Bug, Issue, ... whatever the process template offers
	Title       string
	Description string   // HTML
	Tags        []string // in addition to the marker and id tags
	AreaPath    string   // optional
}

type patch struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func (c *Client) fields(it NewItem) []patch {
	tags := append([]string{MarkerTag, IDTagPrefix + it.FindingID}, it.Tags...)
	ps := []patch{
		{Op: "add", Path: "/fields/System.Title", Value: it.Title},
		{Op: "add", Path: "/fields/System.Tags", Value: strings.Join(tags, "; ")},
	}
	if it.Description != "" {
		ps = append(ps, patch{Op: "add", Path: "/fields/System.Description", Value: it.Description})
	}
	if it.AreaPath != "" {
		ps = append(ps, patch{Op: "add", Path: "/fields/System.AreaPath", Value: it.AreaPath})
	}
	return ps
}

// Create raises a work item and returns it.
func (c *Client) Create(it NewItem) (WorkItem, error) {
	return c.create(it, false)
}

// Validate runs the same create with validateOnly, so a misconfigured token,
// project or work item type is reported before any findings are processed
// rather than halfway through raising them.
func (c *Client) Validate(it NewItem) error {
	_, err := c.create(it, true)
	return err
}

func (c *Client) create(it NewItem, validateOnly bool) (WorkItem, error) {
	endpoint := fmt.Sprintf("%s/%s/_apis/wit/workitems/$%s?api-version=%s",
		c.OrgURL, url.PathEscape(c.Project), url.PathEscape(it.Type), apiVersion)
	if validateOnly {
		endpoint += "&validateOnly=true"
	}
	var res struct {
		ID     int `json:"id"`
		Fields struct {
			State string `json:"System.State"`
			Title string `json:"System.Title"`
		} `json:"fields"`
	}
	// The patch content type is what distinguishes a work item write; sending
	// application/json gets a 400 that does not say why.
	if err := c.do(http.MethodPost, endpoint, c.fields(it), "application/json-patch+json", &res); err != nil {
		return WorkItem{}, err
	}
	return WorkItem{ID: res.ID, State: res.Fields.State, Title: res.Fields.Title, URL: c.WebURL(res.ID)}, nil
}

// Close moves a work item to state and records why in its history. Returns
// false when the item is already in that state, so callers can report what
// actually changed.
//
// There is deliberately no Reopen. A closed item is somebody's finished piece of
// work with its own history; when the same finding comes back it gets a fresh
// item that links to the closed one, rather than that record being reused.
func (c *Client) Close(item WorkItem, state, reason string) (bool, error) {
	if strings.EqualFold(item.State, state) {
		return false, nil
	}
	ps := []patch{{Op: "add", Path: "/fields/System.State", Value: state}}
	if reason != "" {
		ps = append(ps, patch{Op: "add", Path: "/fields/System.History", Value: reason})
	}
	endpoint := fmt.Sprintf("%s/_apis/wit/workitems/%d?api-version=%s", c.OrgURL, item.ID, apiVersion)
	if err := c.do(http.MethodPatch, endpoint, ps, "application/json-patch+json", nil); err != nil {
		return false, err
	}
	return true, nil
}

// HasTag reports whether the item carries tag, case-insensitively — Azure DevOps
// normalises tag case.
func (w WorkItem) HasTag(tag string) bool {
	for _, t := range w.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// Note appends to a work item's history and, in the same patch, adds or removes
// tags. State is untouched: this is how tf-snag says something about an item it
// does not own the lifecycle of.
//
// Tags are sent whole — Azure DevOps has no add/remove operation for them — so
// the list is recomputed from what the item already had.
func (c *Client) Note(item WorkItem, text string, add, remove []string) error {
	ps := []patch{{Op: "add", Path: "/fields/System.History", Value: text}}

	if len(add) > 0 || len(remove) > 0 {
		drop := make(map[string]bool, len(remove))
		for _, t := range remove {
			drop[strings.ToLower(t)] = true
		}
		tags := make([]string, 0, len(item.Tags)+len(add))
		for _, t := range item.Tags {
			if !drop[strings.ToLower(t)] {
				tags = append(tags, t)
			}
		}
		for _, t := range add {
			if !item.HasTag(t) {
				tags = append(tags, t)
			}
		}
		ps = append(ps, patch{Op: "add", Path: "/fields/System.Tags", Value: strings.Join(tags, "; ")})
	}

	endpoint := fmt.Sprintf("%s/_apis/wit/workitems/%d?api-version=%s", c.OrgURL, item.ID, apiVersion)
	return c.do(http.MethodPatch, endpoint, ps, "application/json-patch+json", nil)
}
