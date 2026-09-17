package ado

import (
	"fmt"
	"net/http"
	"net/url"
)

// PRPermission is what a 403 from the thread calls is complaining about. The
// build service identity has it only if somebody granted it: "Contribute to
// pull requests" is separate from "Contribute", and separate again from the work
// item and repository scopes the rest of this package needs.
const PRPermission = "Contribute to pull requests on the repository"

// Thread statuses Azure DevOps accepts. tf-snag uses two: a thread with
// something to say is active, and one whose findings have gone is closed rather
// than deleted, so the PR keeps the history of what was found.
const (
	ThreadActive = "active"
	ThreadClosed = "closed"
)

// PRThread is one comment thread on a pull request.
type PRThread struct {
	ID        int         `json:"id"`
	Status    string      `json:"status"`
	IsDeleted bool        `json:"isDeleted"`
	Comments  []PRComment `json:"comments"`
}

// PRComment is one comment in a thread.
type PRComment struct {
	ID          int    `json:"id"`
	Content     string `json:"content"`
	CommentType string `json:"commentType,omitempty"`
	IsDeleted   bool   `json:"isDeleted,omitempty"`
}

// prThreadList is the collection wrapper every Azure DevOps list endpoint uses.
type prThreadList struct {
	Value []PRThread `json:"value"`
}

// PRThreads lists the comment threads on a pull request, including the ones
// other tools and people left: finding tf-snag's own is the caller's job, since
// what marks it is the comment body it wrote.
func (c *Client) PRThreads(repoID string, prID int) ([]PRThread, error) {
	var out prThreadList
	_, err := c.doWith(prOpts(http.MethodGet, c.prURL(repoID, prID, ""), nil, &out))
	return out.Value, err
}

// CreatePRThread posts a new thread carrying one comment.
func (c *Client) CreatePRThread(repoID string, prID int, content, status string) (PRThread, error) {
	body := map[string]any{
		"comments": []map[string]any{{
			"parentCommentId": 0,
			"content":         content,
			"commentType":     "text",
		}},
		"status": status,
	}
	var out PRThread
	_, err := c.doWith(prOpts(http.MethodPost, c.prURL(repoID, prID, ""), body, &out))
	return out, err
}

// UpdatePRComment rewrites a comment in place, which is what keeps a PR pushed
// to ten times carrying one current comment rather than ten stale ones.
func (c *Client) UpdatePRComment(repoID string, prID, threadID, commentID int, content string) error {
	endpoint := c.prURL(repoID, prID, fmt.Sprintf("/%d/comments/%d", threadID, commentID))
	body := map[string]any{"content": content}
	_, err := c.doWith(prOpts(http.MethodPatch, endpoint, body, nil))
	return err
}

// SetPRThreadStatus closes the thread when the findings have gone, or reopens it
// when they come back.
func (c *Client) SetPRThreadStatus(repoID string, prID, threadID int, status string) error {
	endpoint := c.prURL(repoID, prID, fmt.Sprintf("/%d", threadID))
	_, err := c.doWith(prOpts(http.MethodPatch, endpoint, map[string]any{"status": status}, nil))
	return err
}

func (c *Client) prURL(repoID string, prID int, suffix string) string {
	return fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pullRequests/%d/threads%s?api-version=%s",
		c.OrgURL, url.PathEscape(c.Project), url.PathEscape(repoID), prID, suffix, apiVersion)
}

// prOpts is the shape every thread call shares: the PR permission for a 403, and
// a 404 hint that names both things it can mean, since Azure DevOps answers a
// repository the identity cannot see with 404 rather than 403.
func prOpts(method, endpoint string, body any, out any) reqOpts {
	o := reqOpts{
		method:   method,
		endpoint: endpoint,
		out:      out,
		needs:    PRPermission,
		notFound: "the repository and pull request exist, and that the identity can see them — " +
			"a repository it cannot read answers 404 rather than 403",
	}
	if body != nil {
		o.body = body
		o.contentType = "application/json"
	}
	return o
}
