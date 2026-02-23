package github

import "context"

// GitHubClient is the interface for all GitHub API operations.
// Two implementations exist: AppClient (GitHub App) and PATClient (Personal Access Token).
type GitHubClient interface {
	// GetCloneToken returns a token suitable for git clone over HTTPS.
	GetCloneToken(ctx context.Context) (string, error)

	// CommentOnIssue posts a comment on an issue or PR.
	CommentOnIssue(ctx context.Context, owner, repo string, issueNumber int, body string) error

	// ReplyToReviewComment replies in a PR review comment thread.
	ReplyToReviewComment(ctx context.Context, owner, repo string, prNumber int, commentID int64, body string) error

	// FetchIssueThread returns the formatted issue thread for Claude's prompt.
	FetchIssueThread(ctx context.Context, owner, repo string, number int) (string, error)

	// FetchPRContext returns the formatted PR title and body.
	FetchPRContext(ctx context.Context, owner, repo string, number int) (string, error)

	// FetchReviewCommentThread returns the formatted review comment thread.
	FetchReviewCommentThread(ctx context.Context, owner, repo string, prNumber int, commentID int64) (string, error)

	// CreateRepoWebhook creates a webhook on a repository. Returns the hook ID.
	CreateRepoWebhook(ctx context.Context, owner, repo, webhookURL, secret string) (int64, error)

	// DeleteRepoWebhook deletes a webhook from a repository.
	DeleteRepoWebhook(ctx context.Context, owner, repo string, hookID int64) error

	// GetUserRepoPermission returns the user's permission level (admin, write, read, none).
	GetUserRepoPermission(ctx context.Context, owner, repo, username string) (string, error)

	// BotName returns the bot's display name for @mention filtering.
	BotName() string
}
