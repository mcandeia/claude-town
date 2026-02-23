package github

import (
	"context"
	"fmt"
	"strings"

	gogithub "github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

var _ GitHubClient = (*PATClient)(nil)

// PATClient implements GitHubClient using a Personal Access Token.
type PATClient struct {
	client  *gogithub.Client
	token   string
	botName string
}

// NewPATClient creates a new PATClient authenticated with the given PAT.
func NewPATClient(token, botName string) *PATClient {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return &PATClient{
		client:  gogithub.NewClient(tc),
		token:   token,
		botName: botName,
	}
}

func (c *PATClient) GetCloneToken(_ context.Context) (string, error) {
	return c.token, nil
}

func (c *PATClient) BotName() string {
	return c.botName
}

func (c *PATClient) CommentOnIssue(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	comment := &gogithub.IssueComment{Body: gogithub.Ptr(body)}
	_, _, err := c.client.Issues.CreateComment(ctx, owner, repo, issueNumber, comment)
	if err != nil {
		return fmt.Errorf("creating comment on %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	return nil
}

func (c *PATClient) ReplyToReviewComment(ctx context.Context, owner, repo string, prNumber int, commentID int64, body string) error {
	_, _, err := c.client.PullRequests.CreateCommentInReplyTo(ctx, owner, repo, prNumber, body, commentID)
	if err != nil {
		return fmt.Errorf("replying to review comment %d on %s/%s#%d: %w", commentID, owner, repo, prNumber, err)
	}
	return nil
}

func (c *PATClient) FetchIssueThread(ctx context.Context, owner, repo string, number int) (string, error) {
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("fetching issue #%d: %w", number, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Issue #%d: %s\n\n", number, issue.GetTitle())
	if body := issue.GetBody(); body != "" {
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}

	opts := &gogithub.IssueListCommentsOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	var allComments []*gogithub.IssueComment
	for {
		comments, resp, listErr := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if listErr != nil {
			return "", fmt.Errorf("fetching comments for issue #%d: %w", number, listErr)
		}
		allComments = append(allComments, comments...)
		if resp.NextPage == 0 || len(allComments) >= maxCommentsPerThread {
			break
		}
		opts.Page = resp.NextPage
	}

	var hasComments bool
	for _, comment := range allComments {
		if c.isBotUser(comment.GetUser().GetLogin()) {
			continue
		}
		if !hasComments {
			sb.WriteString("### Comments\n\n")
			hasComments = true
		}
		fmt.Fprintf(&sb, "**@%s** (%s):\n%s\n\n",
			comment.GetUser().GetLogin(),
			comment.GetCreatedAt().Format("2006-01-02"),
			comment.GetBody())
	}

	return truncateThread(sb.String()), nil
}

func (c *PATClient) FetchPRContext(ctx context.Context, owner, repo string, number int) (string, error) {
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("fetching PR #%d: %w", number, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## PR #%d: %s\n\n", number, pr.GetTitle())
	if body := pr.GetBody(); body != "" {
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}
	return sb.String(), nil
}

func (c *PATClient) FetchReviewCommentThread(ctx context.Context, owner, repo string, prNumber int, commentID int64) (string, error) {
	comment, _, err := c.client.PullRequests.GetComment(ctx, owner, repo, commentID)
	if err != nil {
		return "", fmt.Errorf("fetching review comment %d: %w", commentID, err)
	}

	rootID := commentID
	if replyTo := comment.GetInReplyTo(); replyTo != 0 {
		rootID = replyTo
	}

	opts := &gogithub.PullRequestListCommentsOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	var threadComments []*gogithub.PullRequestComment
	for {
		comments, resp, listErr := c.client.PullRequests.ListComments(ctx, owner, repo, prNumber, opts)
		if listErr != nil {
			return "", fmt.Errorf("fetching review comments for PR #%d: %w", prNumber, listErr)
		}
		for _, tc := range comments {
			if tc.GetID() == rootID || tc.GetInReplyTo() == rootID {
				threadComments = append(threadComments, tc)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	var sb strings.Builder
	sb.WriteString("### Review Thread\n\n")

	if len(threadComments) > 0 {
		root := threadComments[0]
		if path := root.GetPath(); path != "" {
			fmt.Fprintf(&sb, "**File:** `%s`\n", path)
		}
		if hunk := root.GetDiffHunk(); hunk != "" {
			fmt.Fprintf(&sb, "```diff\n%s\n```\n\n", hunk)
		}
	}

	for _, tc := range threadComments {
		if c.isBotUser(tc.GetUser().GetLogin()) {
			continue
		}
		fmt.Fprintf(&sb, "**@%s** (%s):\n%s\n\n",
			tc.GetUser().GetLogin(),
			tc.GetCreatedAt().Format("2006-01-02"),
			tc.GetBody())
	}

	return truncateThread(sb.String()), nil
}

func (c *PATClient) CreateRepoWebhook(ctx context.Context, owner, repo, webhookURL, secret string) (int64, error) {
	hook := &gogithub.Hook{
		Config: &gogithub.HookConfig{
			ContentType: gogithub.Ptr("json"),
			URL:         gogithub.Ptr(webhookURL),
			Secret:      gogithub.Ptr(secret),
			InsecureSSL: gogithub.Ptr("0"),
		},
		Events: []string{"issues", "issue_comment", "pull_request_review", "pull_request_review_comment"},
		Active: gogithub.Ptr(true),
	}

	created, _, err := c.client.Repositories.CreateHook(ctx, owner, repo, hook)
	if err != nil {
		return 0, fmt.Errorf("creating webhook on %s/%s: %w", owner, repo, err)
	}
	return created.GetID(), nil
}

func (c *PATClient) DeleteRepoWebhook(ctx context.Context, owner, repo string, hookID int64) error {
	_, err := c.client.Repositories.DeleteHook(ctx, owner, repo, hookID)
	if err != nil {
		return fmt.Errorf("deleting webhook %d on %s/%s: %w", hookID, owner, repo, err)
	}
	return nil
}

func (c *PATClient) GetUserRepoPermission(ctx context.Context, owner, repo, username string) (string, error) {
	perm, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, repo, username)
	if err != nil {
		return "", fmt.Errorf("getting permission for %s on %s/%s: %w", username, owner, repo, err)
	}
	return perm.GetPermission(), nil
}

func (c *PATClient) isBotUser(login string) bool {
	return login == c.botName || login == c.botName+"[bot]"
}
