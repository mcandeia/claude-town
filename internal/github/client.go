package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gogithub "github.com/google/go-github/v68/github"
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

// GetInstallationToken creates a short-lived installation access token that can
// be used for git operations (clone, push, etc.) over HTTPS.
func (c *Client) GetInstallationToken(ctx context.Context) (string, error) {
	token, _, err := c.app.Apps.CreateInstallationToken(ctx, c.config.InstallationID, nil)
	if err != nil {
		return "", fmt.Errorf("creating installation token: %w", err)
	}

	return token.GetToken(), nil
}
