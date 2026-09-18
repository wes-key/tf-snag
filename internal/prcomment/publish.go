package prcomment

import (
	"fmt"
	"io"
	"strings"

	"github.com/wes-key/tf-snag/internal/ado"
	"github.com/wes-key/tf-snag/internal/report"
)

// Poster is the subset of *ado.Client this package needs, so the publish logic
// can be tested without an Azure DevOps instance.
type Poster interface {
	PRThreads(repoID string, prID int) ([]ado.PRThread, error)
	CreatePRThread(repoID string, prID int, content, status string) (ado.PRThread, error)
	UpdatePRComment(repoID string, prID, threadID, commentID int, content string) error
	SetPRThreadStatus(repoID string, prID, threadID int, status string) error
}

// Target is the pull request to comment on.
type Target struct {
	// RepoID is the repository id or name, e.g. $(Build.Repository.ID).
	RepoID string
	// PullRequest is the id, e.g. $(System.PullRequest.PullRequestId).
	PullRequest int
}

// Result says what happened, for the caller's log.
type Result struct {
	Created  bool
	Updated  bool
	Resolved bool // the thread was closed because the findings have gone
	Skipped  bool // nothing to say and no thread to update
	ThreadID int
}

// ShouldPost decides whether this run warrants a comment.
//
//	findings  anything un-suppressed was found
//	new       only a finding this pull request adds, or one that has just come
//	          out from under an ignore rule
//
// Under "new" a pull request that changes nothing tf-snag can see stays silent,
// which is the difference between a check people read and one they filter out.
// It needs -baseline to tell a new finding from a long-standing one; without one
// it falls back to "findings" and says so, because a comment that silently never
// arrives is the worst failure available to it.
func ShouldPost(rep *report.Report, mode string, log io.Writer) bool {
	switch mode {
	case "new":
		if !rep.IsBaselined() {
			fmt.Fprintln(log, "tf-snag: -pr-comment new needs -baseline to identify new findings; commenting as -pr-comment findings would")
			return rep.HasGatingFindings()
		}
		return rep.HasNewlyActionable()
	default: // findings
		return rep.HasGatingFindings()
	}
}

// Publish puts body on the pull request as tf-snag's own thread.
//
// One thread per pull request, found by the marker in the body it wrote last
// time: a pull request pushed to ten times carries one comment showing the
// current state, not ten stale ones. When the findings have gone the thread is
// updated to say so and closed rather than deleted, so the history of what was
// found stays readable.
//
// dryRun reports what would happen and posts nothing.
func Publish(p Poster, t Target, body string, clean, dryRun bool, log io.Writer) (Result, error) {
	threads, err := p.PRThreads(t.RepoID, t.PullRequest)
	if err != nil {
		return Result{}, fmt.Errorf("reading the pull request's comments: %w", err)
	}
	thread, comment, found := findMarked(threads)

	// Nothing to say and nothing said before: post no thread at all. A clean
	// pull request should not collect a comment saying so.
	if !found && clean {
		fmt.Fprintln(log, "pr: nothing to report and no existing comment — posting nothing")
		return Result{Skipped: true}, nil
	}

	status := ado.ThreadActive
	if clean {
		status = ado.ThreadClosed
	}

	if !found {
		if dryRun {
			fmt.Fprintf(log, "pr: would post a new comment on !%d\n", t.PullRequest)
			return Result{Created: true}, nil
		}
		created, err := p.CreatePRThread(t.RepoID, t.PullRequest, body, status)
		if err != nil {
			return Result{}, fmt.Errorf("posting the comment: %w", err)
		}
		fmt.Fprintf(log, "pr: commented on !%d (thread %d)\n", t.PullRequest, created.ID)
		return Result{Created: true, ThreadID: created.ID}, nil
	}

	if dryRun {
		fmt.Fprintf(log, "pr: would update comment %d in thread %d on !%d\n", comment.ID, thread.ID, t.PullRequest)
		return Result{Updated: true, ThreadID: thread.ID}, nil
	}
	if err := p.UpdatePRComment(t.RepoID, t.PullRequest, thread.ID, comment.ID, body); err != nil {
		return Result{}, fmt.Errorf("updating the comment: %w", err)
	}
	res := Result{Updated: true, ThreadID: thread.ID}

	// Only move the status when it is wrong: reopening a thread somebody closed
	// by hand, on a run that found nothing, would be tf-snag arguing with a
	// reviewer.
	if !strings.EqualFold(thread.Status, status) {
		if err := p.SetPRThreadStatus(t.RepoID, t.PullRequest, thread.ID, status); err != nil {
			return res, fmt.Errorf("setting the comment thread to %s: %w", status, err)
		}
		res.Resolved = clean
	}
	fmt.Fprintf(log, "pr: updated the comment on !%d (thread %d)\n", t.PullRequest, thread.ID)
	return res, nil
}

// findMarked returns tf-snag's thread and the comment carrying the marker.
//
// Deleted threads and comments are skipped: a reviewer who deleted the comment
// has said they do not want it, and editing it back would be a fight. A fresh
// thread is posted instead, which is the reviewer's own doing.
func findMarked(threads []ado.PRThread) (ado.PRThread, ado.PRComment, bool) {
	for _, t := range threads {
		if t.IsDeleted {
			continue
		}
		for _, c := range t.Comments {
			if !c.IsDeleted && strings.Contains(c.Content, Marker) {
				return t, c, true
			}
		}
	}
	return ado.PRThread{}, ado.PRComment{}, false
}
