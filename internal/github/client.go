package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gogithub "github.com/google/go-github/v68/github"
)

const (
	// maxThreadChars is the truncation limit for thread context to avoid overwhelming Claude.
	maxThreadChars = 50000
	// maxCommentsPerThread caps the number of comments fetched per thread.
	maxCommentsPerThread = 50
)

// Config holds the configuration for a GitHub App.
type Config struct {
	// AppID is the GitHub App's unique identifier.
	AppID int64
	// InstallationID is the installation ID for the target organization or user.
	InstallationID int64
	// PrivateKey is the PEM-encoded private key for the GitHub App.
	PrivateKey []byte
	// WebhookSecret is the secret used to verify incoming webhook payloads.
	WebhookSecret string
	// SelfDNS is the public DNS name where this service is reachable.
	SelfDNS string
	// BotName is the name of the GitHub App bot user (e.g. "claude-town[bot]").
	BotName string
}

// WebhookURL returns the full webhook URL that GitHub should deliver events to.
func (c Config) WebhookURL() string {
	return fmt.Sprintf("https://%s/webhooks/github", c.SelfDNS)
}

// Client wraps two GitHub API clients: one authenticated as the App (for
// app-level operations like managing webhooks and creating installation tokens)
// and one authenticated as an Installation (for repository-scoped operations
// like commenting on issues).
type Client struct {
	app          *gogithub.Client
	installation *gogithub.Client
	config       Config
}

// NewClient creates a new Client with both app-level and installation-level
// GitHub API clients. The app transport is used for operations that require
// app-level authentication (e.g. webhook config, installation tokens) while
// the installation transport is used for repository-scoped operations.
func NewClient(cfg Config) (*Client, error) {
	appTransport, err := ghinstallation.NewAppsTransport(http.DefaultTransport, cfg.AppID, cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("creating app transport: %w", err)
	}

	installTransport, err := ghinstallation.New(http.DefaultTransport, cfg.AppID, cfg.InstallationID, cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("creating installation transport: %w", err)
	}

	appClient := gogithub.NewClient(&http.Client{Transport: appTransport})
	installClient := gogithub.NewClient(&http.Client{Transport: installTransport})

	return &Client{
		app:          appClient,
		installation: installClient,
		config:       cfg,
	}, nil
}

// BotName returns the configured bot name for this GitHub App.
func (c *Client) BotName() string {
	return c.config.BotName
}

// RegisterWebhook updates the GitHub App's webhook configuration to point at
// this service's webhook endpoint. It sets the URL, content type, and secret.
//
// Note: webhook event subscriptions (issue_comment, pull_request_review,
// pull_request_review_comment) must be configured in the GitHub App settings
// or via the app manifest. The API only allows updating the hook delivery
// configuration (URL, content type, secret).
func (c *Client) RegisterWebhook(ctx context.Context) error {
	hookConfig := &gogithub.HookConfig{
		ContentType: gogithub.Ptr("json"),
		URL:         gogithub.Ptr(c.config.WebhookURL()),
		Secret:      gogithub.Ptr(c.config.WebhookSecret),
		InsecureSSL: gogithub.Ptr("0"),
	}

	_, _, err := c.app.Apps.UpdateHookConfig(ctx, hookConfig)
	if err != nil {
		return fmt.Errorf("updating webhook config: %w", err)
	}

	return nil
}

// CommentOnIssue posts a comment on the specified issue (or pull request) using
// the installation-level client.
func (c *Client) CommentOnIssue(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	comment := &gogithub.IssueComment{
		Body: gogithub.Ptr(body),
	}

	_, _, err := c.installation.Issues.CreateComment(ctx, owner, repo, issueNumber, comment)
	if err != nil {
		return fmt.Errorf("creating comment on %s/%s#%d: %w", owner, repo, issueNumber, err)
	}

	return nil
}

// ReplyToReviewComment posts a reply to a specific PR review comment thread.
func (c *Client) ReplyToReviewComment(ctx context.Context, owner, repo string, prNumber int, commentID int64, body string) error {
	_, _, err := c.installation.PullRequests.CreateCommentInReplyTo(ctx, owner, repo, prNumber, body, commentID)
	if err != nil {
		return fmt.Errorf("replying to review comment %d on %s/%s#%d: %w", commentID, owner, repo, prNumber, err)
	}

	return nil
}

// GetInstallationToken creates a short-lived installation access token that can
// be used for git operations (clone, push, etc.) over HTTPS.
func (c *Client) GetInstallationToken(ctx context.Context) (string, error) {
	token, _, err := c.app.Apps.CreateInstallationToken(ctx, c.config.InstallationID, nil)
	if err != nil {
		return "", fmt.Errorf("creating installation token: %w", err)
	}

	return token.GetToken(), nil
}

// FetchIssueThread fetches the full issue thread (title, body, and comments)
// formatted as markdown for inclusion in a Claude prompt.
func (c *Client) FetchIssueThread(ctx context.Context, owner, repo string, number int) (string, error) {
	issue, _, err := c.installation.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("fetching issue #%d: %w", number, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Issue #%d: %s\n\n", number, issue.GetTitle())
	if body := issue.GetBody(); body != "" {
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}

	// Fetch comments with pagination.
	opts := &gogithub.IssueListCommentsOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	var allComments []*gogithub.IssueComment
	for {
		comments, resp, err := c.installation.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return "", fmt.Errorf("fetching comments for issue #%d: %w", number, err)
		}
		allComments = append(allComments, comments...)
		if resp.NextPage == 0 || len(allComments) >= maxCommentsPerThread {
			break
		}
		opts.Page = resp.NextPage
	}

	// Filter out bot comments and format.
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

// FetchPRContext fetches the PR title and body formatted as markdown.
func (c *Client) FetchPRContext(ctx context.Context, owner, repo string, number int) (string, error) {
	pr, _, err := c.installation.PullRequests.Get(ctx, owner, repo, number)
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

// FetchReviewCommentThread fetches the full review comment thread for a given
// comment ID, including the file path, diff hunk, and all replies.
func (c *Client) FetchReviewCommentThread(ctx context.Context, owner, repo string, prNumber int, commentID int64) (string, error) {
	// Get the target comment to find the thread root.
	comment, _, err := c.installation.PullRequests.GetComment(ctx, owner, repo, commentID)
	if err != nil {
		return "", fmt.Errorf("fetching review comment %d: %w", commentID, err)
	}

	rootID := commentID
	if replyTo := comment.GetInReplyTo(); replyTo != 0 {
		rootID = replyTo
	}

	// Fetch all review comments on the PR and filter to this thread.
	opts := &gogithub.PullRequestListCommentsOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	var threadComments []*gogithub.PullRequestComment
	for {
		comments, resp, err := c.installation.PullRequests.ListComments(ctx, owner, repo, prNumber, opts)
		if err != nil {
			return "", fmt.Errorf("fetching review comments for PR #%d: %w", prNumber, err)
		}
		for _, c := range comments {
			if c.GetID() == rootID || c.GetInReplyTo() == rootID {
				threadComments = append(threadComments, c)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	var sb strings.Builder
	sb.WriteString("### Review Thread\n\n")

	// Include file path and diff hunk from the root comment (first in thread).
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

// isBotUser returns true if the login matches the configured bot name.
func (c *Client) isBotUser(login string) bool {
	return login == c.config.BotName || login == c.config.BotName+"[bot]"
}

// truncateThread truncates the string to maxThreadChars, appending a notice.
func truncateThread(s string) string {
	if len(s) <= maxThreadChars {
		return s
	}
	return s[:maxThreadChars] + "\n\n... (thread truncated)"
}
