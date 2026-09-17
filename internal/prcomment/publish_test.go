package prcomment

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wes-key/tf-snag/internal/ado"
	"github.com/wes-key/tf-snag/internal/report"
)

// fakePoster records what Publish did, so the tests assert on behaviour rather
// than on an Azure DevOps instance.
type fakePoster struct {
	threads []ado.PRThread
	err     error

	created  []string
	updated  []string
	statuses []string
}

func (f *fakePoster) PRThreads(string, int) ([]ado.PRThread, error) {
	return f.threads, f.err
}

func (f *fakePoster) CreatePRThread(_ string, _ int, content, status string) (ado.PRThread, error) {
	f.created = append(f.created, content)
	f.statuses = append(f.statuses, status)
	return ado.PRThread{ID: 99}, nil
}

func (f *fakePoster) UpdatePRComment(_ string, _, _, _ int, content string) error {
	f.updated = append(f.updated, content)
	return nil
}

func (f *fakePoster) SetPRThreadStatus(_ string, _, _ int, status string) error {
	f.statuses = append(f.statuses, status)
	return nil
}

var target = Target{RepoID: "repo-1", PullRequest: 7}

func TestPublishPostsANewThread(t *testing.T) {
	f := &fakePoster{}

	res, err := Publish(f, target, "body "+Marker, false, false, io.Discard)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Created || res.ThreadID != 99 {
		t.Errorf("Result = %+v, want a created thread 99", res)
	}
	if len(f.created) != 1 || len(f.updated) != 0 {
		t.Errorf("created %d, updated %d; want one create", len(f.created), len(f.updated))
	}
	if f.statuses[0] != ado.ThreadActive {
		t.Errorf("new thread status = %q, want %q", f.statuses[0], ado.ThreadActive)
	}
}

// The whole point of the marker: ten pushes leave one comment, not ten.
func TestPublishUpdatesItsOwnThread(t *testing.T) {
	f := &fakePoster{threads: []ado.PRThread{
		{ID: 1, Status: ado.ThreadActive, Comments: []ado.PRComment{{ID: 10, Content: "a reviewer said something"}}},
		{ID: 2, Status: ado.ThreadActive, Comments: []ado.PRComment{{ID: 20, Content: "older report " + Marker}}},
	}}

	res, err := Publish(f, target, "fresh "+Marker, false, false, io.Discard)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Updated || res.ThreadID != 2 {
		t.Errorf("Result = %+v, want thread 2 updated", res)
	}
	if len(f.created) != 0 {
		t.Errorf("posted a second thread: %q", f.created)
	}
	if len(f.updated) != 1 || f.updated[0] != "fresh "+Marker {
		t.Errorf("updated = %q, want the new body", f.updated)
	}
}

func TestPublishSaysNothingWhenThereIsNothingToSay(t *testing.T) {
	f := &fakePoster{}

	res, err := Publish(f, target, "body "+Marker, true, false, io.Discard)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Skipped || len(f.created) != 0 {
		t.Errorf("Result = %+v, created %q; want nothing posted on a clean run", res, f.created)
	}
}

// A run that fixes everything has to update the comment, or the pull request
// keeps claiming findings that have gone.
func TestPublishClosesTheThreadWhenTheFindingsHaveGone(t *testing.T) {
	f := &fakePoster{threads: []ado.PRThread{
		{ID: 3, Status: ado.ThreadActive, Comments: []ado.PRComment{{ID: 30, Content: "stale " + Marker}}},
	}}

	res, err := Publish(f, target, "all clear "+Marker, true, false, io.Discard)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Updated || !res.Resolved {
		t.Errorf("Result = %+v, want the thread updated and resolved", res)
	}
	if len(f.statuses) != 1 || f.statuses[0] != ado.ThreadClosed {
		t.Errorf("statuses = %v, want one %q", f.statuses, ado.ThreadClosed)
	}
}

// Moving a status that is already right would show up as tf-snag activity on a
// pull request where nothing changed.
func TestPublishLeavesAnAlreadyCorrectStatusAlone(t *testing.T) {
	f := &fakePoster{threads: []ado.PRThread{
		{ID: 4, Status: "Closed", Comments: []ado.PRComment{{ID: 40, Content: "stale " + Marker}}},
	}}

	if _, err := Publish(f, target, "all clear "+Marker, true, false, io.Discard); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(f.statuses) != 0 {
		t.Errorf("set the status to %v when it was already closed", f.statuses)
	}
}

// A reviewer who deleted the comment has said they do not want it. Editing it
// back would be a fight; a new thread is their own doing.
func TestPublishIgnoresDeletedThreadsAndComments(t *testing.T) {
	f := &fakePoster{threads: []ado.PRThread{
		{ID: 5, IsDeleted: true, Comments: []ado.PRComment{{ID: 50, Content: "gone " + Marker}}},
		{ID: 6, Comments: []ado.PRComment{{ID: 60, Content: "also gone " + Marker, IsDeleted: true}}},
	}}

	res, err := Publish(f, target, "body "+Marker, false, false, io.Discard)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Created {
		t.Errorf("Result = %+v, want a new thread when the old one was deleted", res)
	}
}

func TestPublishDryRunChangesNothing(t *testing.T) {
	f := &fakePoster{threads: []ado.PRThread{
		{ID: 7, Status: ado.ThreadActive, Comments: []ado.PRComment{{ID: 70, Content: "old " + Marker}}},
	}}
	var log strings.Builder

	res, err := Publish(f, target, "new "+Marker, false, true, &log)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Updated {
		t.Errorf("Result = %+v, want the update it would have made", res)
	}
	if len(f.created)+len(f.updated)+len(f.statuses) != 0 {
		t.Errorf("dry run wrote something: created %q, updated %q, statuses %v", f.created, f.updated, f.statuses)
	}
	if !strings.Contains(log.String(), "would update") {
		t.Errorf("dry run did not say what it would do: %q", log.String())
	}
}

func TestPublishReportsAReadFailure(t *testing.T) {
	f := &fakePoster{err: errors.New("403")}

	if _, err := Publish(f, target, "body", false, false, io.Discard); err == nil {
		t.Fatal("Publish succeeded despite failing to read the threads")
	} else if !strings.Contains(err.Error(), "pull request's comments") {
		t.Errorf("error = %v, want it to name what failed", err)
	}
}

func TestShouldPost(t *testing.T) {
	findings := &report.Report{Drift: []report.ResourceReport{{Address: "a", Action: "Updated"}}}
	ignored := &report.Report{Drift: []report.ResourceReport{{Address: "a", Action: "Updated", Suppressed: true}}}

	for _, tc := range []struct {
		name string
		rep  *report.Report
		mode string
		want bool
	}{
		{"findings with something to say", findings, "findings", true},
		{"findings with only ignored", ignored, "findings", false},
		{"new without a baseline falls back to findings", findings, "new", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldPost(tc.rep, tc.mode, io.Discard); got != tc.want {
				t.Errorf("ShouldPost = %v, want %v", got, tc.want)
			}
		})
	}
}

// Falling back silently would leave somebody believing a check is running that
// never fires.
func TestShouldPostSaysWhenItFallsBack(t *testing.T) {
	var log strings.Builder
	ShouldPost(&report.Report{}, "new", &log)

	if !strings.Contains(log.String(), "needs -baseline") {
		t.Errorf("fallback is silent: %q", log.String())
	}
}
