/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TaskCreator is the interface for creating tasks from webhook events.
type TaskCreator interface {
	// CreateTask creates a new ClaudeTask from a webhook event.
	CreateTask(ctx context.Context, req TaskRequest) error
	// IsRepoAllowed checks whether the given owner/repo has a ClaudeRepository resource.
	IsRepoAllowed(ctx context.Context, owner, repo string) (bool, error)
	// ResumeClarification checks if a WaitingForClarification task exists for the
	// given repo+issue and, if so, fills in the answer and resumes it.
	ResumeClarification(ctx context.Context, req TaskRequest) (bool, error)
}

// TaskRequest holds the information extracted from a GitHub webhook event
// needed to create a ClaudeTask.
type TaskRequest struct {
	Owner               string
	Repo                string
	Issue               int
	PullRequest         int
	TaskType            string
	Branch              string
	Prompt              string
	ReviewCommentID     int64
	ClarificationAnswer string
}

// Handler is an http.Handler that processes GitHub webhook events.
type Handler struct {
	webhookSecret string
	botName       string
	creator       TaskCreator
}

// NewHandler returns a new webhook Handler.
func NewHandler(webhookSecret, botName string, creator TaskCreator) *Handler {
	return &Handler{
		webhookSecret: webhookSecret,
		botName:       botName,
		creator:       creator,
	}
}

// HasBotMention returns true if body contains "@botName".
func HasBotMention(body, botName string) bool {
	return strings.Contains(body, "@"+botName)
}

// isBotComment returns true if the comment author is the bot itself.
func isBotComment(login, botName string) bool {
	return login == botName || login == botName+"[bot]"
}

// ServeHTTP implements http.Handler. It validates the request method, reads the
// body, validates the webhook signature, and routes by X-GitHub-Event header.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error(err, "failed to read request body")
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	if !validateSignature(body, signature, h.webhookSecret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	logger.Info("received webhook", "event", event)

	switch event {
	case "issue_comment":
		h.handleIssueComment(r.Context(), w, body)
	case "pull_request_review":
		h.handlePRReview(r.Context(), w, body)
	case "pull_request_review_comment":
		h.handlePRReviewComment(r.Context(), w, body)
	default:
		logger.Info("ignoring unhandled event", "event", event)
		w.WriteHeader(http.StatusOK)
	}
}

// issueCommentEvent represents the relevant fields of a GitHub issue_comment event.
type issueCommentEvent struct {
	Action  string `json:"action"`
	Comment struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number      int `json:"number"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"issue"`
	Repository struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}

// handleIssueComment handles an issue_comment webhook event.
func (h *Handler) handleIssueComment(ctx context.Context, w http.ResponseWriter, body []byte) {
	logger := log.FromContext(ctx)

	var event issueCommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		logger.Error(err, "failed to parse issue_comment event")
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if event.Action != "created" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !HasBotMention(event.Comment.Body, h.botName) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ignore comments from the bot itself.
	if isBotComment(event.Comment.User.Login, h.botName) {
		logger.Info("ignoring bot's own comment")
		w.WriteHeader(http.StatusOK)
		return
	}

	owner := event.Repository.Owner.Login
	repo := event.Repository.Name

	allowed, err := h.creator.IsRepoAllowed(ctx, owner, repo)
	if err != nil {
		logger.Error(err, "failed to check repo allowlist", "owner", owner, "repo", repo)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		logger.Info("repo not allowed", "owner", owner, "repo", repo)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Try to resume a task waiting for clarification.
	issueNum := event.Issue.Number
	resumed, err := h.creator.ResumeClarification(ctx, TaskRequest{
		Owner: owner, Repo: repo, Issue: issueNum,
		ClarificationAnswer: event.Comment.Body,
	})
	if err != nil {
		logger.Error(err, "failed to check for clarification task")
	} else if resumed {
		logger.Info("resumed clarification task", "owner", owner, "repo", repo, "issue", issueNum)
		w.WriteHeader(http.StatusOK)
		return
	}

	req := TaskRequest{
		Owner:    owner,
		Repo:     repo,
		Issue:    issueNum,
		TaskType: "IssueSolve",
	}

	if err := h.creator.CreateTask(ctx, req); err != nil {
		logger.Error(err, "failed to create task", "owner", owner, "repo", repo, "issue", req.Issue)
		http.Error(w, "failed to create task", http.StatusInternalServerError)
		return
	}

	logger.Info("created IssueSolve task", "owner", owner, "repo", repo, "issue", req.Issue)
	w.WriteHeader(http.StatusCreated)
}

// pullRequestReviewEvent represents the relevant fields of a GitHub
// pull_request_review event.
type pullRequestReviewEvent struct {
	Action string `json:"action"`
	Review struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"review"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}

// handlePRReview handles a pull_request_review webhook event.
func (h *Handler) handlePRReview(ctx context.Context, w http.ResponseWriter, body []byte) {
	logger := log.FromContext(ctx)

	var event pullRequestReviewEvent
	if err := json.Unmarshal(body, &event); err != nil {
		logger.Error(err, "failed to parse pull_request_review event")
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if event.Action != "submitted" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !HasBotMention(event.Review.Body, h.botName) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ignore reviews from the bot itself.
	if isBotComment(event.Review.User.Login, h.botName) {
		logger.Info("ignoring bot's own review")
		w.WriteHeader(http.StatusOK)
		return
	}

	owner := event.Repository.Owner.Login
	repo := event.Repository.Name

	allowed, err := h.creator.IsRepoAllowed(ctx, owner, repo)
	if err != nil {
		logger.Error(err, "failed to check repo allowlist", "owner", owner, "repo", repo)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		logger.Info("repo not allowed", "owner", owner, "repo", repo)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Try to resume a task waiting for clarification.
	prNum := event.PullRequest.Number
	resumed, err := h.creator.ResumeClarification(ctx, TaskRequest{
		Owner: owner, Repo: repo, PullRequest: prNum,
		ClarificationAnswer: event.Review.Body,
	})
	if err != nil {
		logger.Error(err, "failed to check for clarification task")
	} else if resumed {
		logger.Info("resumed clarification task", "owner", owner, "repo", repo, "pr", prNum)
		w.WriteHeader(http.StatusOK)
		return
	}

	req := TaskRequest{
		Owner:       owner,
		Repo:        repo,
		PullRequest: prNum,
		TaskType:    "PRReviewFix",
		Branch:      event.PullRequest.Head.Ref,
	}

	if err := h.creator.CreateTask(ctx, req); err != nil {
		logger.Error(err, "failed to create task", "owner", owner, "repo", repo, "pr", req.PullRequest)
		http.Error(w, "failed to create task", http.StatusInternalServerError)
		return
	}

	logger.Info("created PRReviewFix task", "owner", owner, "repo", repo, "pr", req.PullRequest)
	w.WriteHeader(http.StatusCreated)
}

// pullRequestReviewCommentEvent represents the relevant fields of a GitHub
// pull_request_review_comment event.
type pullRequestReviewCommentEvent struct {
	Action  string `json:"action"`
	Comment struct {
		ID       int64  `json:"id"`
		Body     string `json:"body"`
		Path     string `json:"path"`
		Line     int    `json:"line"`
		DiffHunk string `json:"diff_hunk"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}

// handlePRReviewComment handles a pull_request_review_comment webhook event.
func (h *Handler) handlePRReviewComment(ctx context.Context, w http.ResponseWriter, body []byte) {
	logger := log.FromContext(ctx)

	var event pullRequestReviewCommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		logger.Error(err, "failed to parse pull_request_review_comment event")
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if event.Action != "created" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !HasBotMention(event.Comment.Body, h.botName) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ignore comments from the bot itself.
	if isBotComment(event.Comment.User.Login, h.botName) {
		logger.Info("ignoring bot's own review comment")
		w.WriteHeader(http.StatusOK)
		return
	}

	owner := event.Repository.Owner.Login
	repo := event.Repository.Name

	allowed, err := h.creator.IsRepoAllowed(ctx, owner, repo)
	if err != nil {
		logger.Error(err, "failed to check repo allowlist", "owner", owner, "repo", repo)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		logger.Info("repo not allowed", "owner", owner, "repo", repo)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Try to resume a task waiting for clarification.
	prNum := event.PullRequest.Number
	resumed, err := h.creator.ResumeClarification(ctx, TaskRequest{
		Owner: owner, Repo: repo, PullRequest: prNum,
		ClarificationAnswer: event.Comment.Body,
	})
	if err != nil {
		logger.Error(err, "failed to check for clarification task")
	} else if resumed {
		logger.Info("resumed clarification task", "owner", owner, "repo", repo, "pr", prNum)
		w.WriteHeader(http.StatusOK)
		return
	}

	prompt := buildReviewCommentPrompt(
		event.PullRequest.Number,
		event.Comment.Path,
		event.Comment.Line,
		event.Comment.DiffHunk,
		event.Comment.Body,
	)

	req := TaskRequest{
		Owner:           owner,
		Repo:            repo,
		PullRequest:     event.PullRequest.Number,
		TaskType:        "PRReviewFix",
		Branch:          event.PullRequest.Head.Ref,
		Prompt:          prompt,
		ReviewCommentID: event.Comment.ID,
	}

	if err := h.creator.CreateTask(ctx, req); err != nil {
		logger.Error(err, "failed to create task", "owner", owner, "repo", repo, "pr", req.PullRequest)
		http.Error(w, "failed to create task", http.StatusInternalServerError)
		return
	}

	logger.Info("created PRReviewFix task", "owner", owner, "repo", repo, "pr", req.PullRequest, "file", event.Comment.Path)
	w.WriteHeader(http.StatusCreated)
}

// buildReviewCommentPrompt constructs a prompt with the file, line, diff, and
// comment context from a PR review comment.
func buildReviewCommentPrompt(pr int, path string, line int, diffHunk, comment string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Fix the review comment on PR #%d.\n\n", pr)
	if path != "" {
		fmt.Fprintf(&sb, "The reviewer commented on file `%s`", path)
		if line > 0 {
			fmt.Fprintf(&sb, " at line %d", line)
		}
		sb.WriteString(":\n\n")
	}
	if diffHunk != "" {
		sb.WriteString("```diff\n")
		sb.WriteString(diffHunk)
		sb.WriteString("\n```\n\n")
	}
	fmt.Fprintf(&sb, "Reviewer said: %q\n\n", comment)
	sb.WriteString("Fix this specific issue, commit, and push to the same branch.")
	return sb.String()
}

// validateSignature checks the HMAC-SHA256 signature of the payload.
// If secret is empty, signature validation is skipped (useful for development).
func validateSignature(body []byte, signature, secret string) bool {
	if secret == "" {
		return true
	}

	if signature == "" {
		return false
	}

	// The signature header has the format "sha256=<hex-digest>"
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}

	sigBytes, err := hex.DecodeString(signature[len(prefix):])
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	return hmac.Equal(sigBytes, expected)
}

// computeSignature computes the HMAC-SHA256 signature for a payload and secret.
// It returns the signature in the "sha256=<hex>" format used by GitHub.
func computeSignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return fmt.Sprintf("sha256=%s", hex.EncodeToString(mac.Sum(nil)))
}
