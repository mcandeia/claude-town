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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockCreator is a test double for TaskCreator.
type mockCreator struct {
	allowed      bool
	allowedErr   error
	createCalled bool
	lastRequest  TaskRequest
	createErr    error
	resumeCalled bool
	resumeResult bool
	resumeErr    error
}

func (m *mockCreator) CreateTask(_ context.Context, req TaskRequest) error {
	m.createCalled = true
	m.lastRequest = req
	return m.createErr
}

func (m *mockCreator) IsRepoAllowed(_ context.Context, _, _ string) (bool, error) {
	return m.allowed, m.allowedErr
}

func (m *mockCreator) ResumeClarification(_ context.Context, req TaskRequest) (bool, error) {
	m.resumeCalled = true
	m.lastRequest = req
	return m.resumeResult, m.resumeErr
}

func TestParseMention(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		botName string
		want    bool
	}{
		{
			name:    "mention present",
			body:    "Hey @claude-town please fix this issue",
			botName: "claude-town",
			want:    true,
		},
		{
			name:    "no mention",
			body:    "This is a regular comment with no mention",
			botName: "claude-town",
			want:    false,
		},
		{
			name:    "different bot mentioned",
			body:    "Hey @other-bot please help",
			botName: "claude-town",
			want:    false,
		},
		{
			name:    "mention at start of body",
			body:    "@claude-town do something",
			botName: "claude-town",
			want:    true,
		},
		{
			name:    "mention at end of body",
			body:    "Please help @claude-town",
			botName: "claude-town",
			want:    true,
		},
		{
			name:    "empty body",
			body:    "",
			botName: "claude-town",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasBotMention(tt.body, tt.botName)
			if got != tt.want {
				t.Errorf("HasBotMention(%q, %q) = %v, want %v", tt.body, tt.botName, got, tt.want)
			}
		})
	}
}

func TestWebhookHandler_InvalidSignature(t *testing.T) {
	creator := &mockCreator{allowed: true}
	handler := NewHandler("my-secret", "claude-town", creator)

	body, _ := json.Marshal(map[string]string{"action": "created"})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidsignature")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestWebhookHandler_ValidSignature_IssueComment(t *testing.T) {
	creator := &mockCreator{allowed: true}
	secret := "test-secret"
	handler := NewHandler(secret, "claude-town", creator)

	payload := map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"body": "Hey @claude-town fix this",
		},
		"issue": map[string]interface{}{
			"number": 42,
		},
		"repository": map[string]interface{}{
			"owner": map[string]interface{}{
				"login": "myorg",
			},
			"name": "myrepo",
		},
	}
	body, _ := json.Marshal(payload)

	sig := computeSignature(body, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, rr.Code)
	}

	if !creator.createCalled {
		t.Fatal("expected CreateTask to be called")
	}

	if creator.lastRequest.Owner != "myorg" {
		t.Errorf("expected owner %q, got %q", "myorg", creator.lastRequest.Owner)
	}
	if creator.lastRequest.Repo != "myrepo" {
		t.Errorf("expected repo %q, got %q", "myrepo", creator.lastRequest.Repo)
	}
	if creator.lastRequest.Issue != 42 {
		t.Errorf("expected issue %d, got %d", 42, creator.lastRequest.Issue)
	}
	if creator.lastRequest.TaskType != "IssueSolve" {
		t.Errorf("expected task type %q, got %q", "IssueSolve", creator.lastRequest.TaskType)
	}
}

func TestWebhookHandler_MethodNotAllowed(t *testing.T) {
	creator := &mockCreator{allowed: true}
	handler := NewHandler("", "claude-town", creator)

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}

func TestWebhookHandler_NoSecret_SkipsValidation(t *testing.T) {
	creator := &mockCreator{allowed: true}
	handler := NewHandler("", "claude-town", creator)

	payload := map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"body": "@claude-town help",
		},
		"issue": map[string]interface{}{
			"number": 7,
		},
		"repository": map[string]interface{}{
			"owner": map[string]interface{}{
				"login": "org",
			},
			"name": "repo",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	// No signature header set; secret is empty so validation is skipped.

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, rr.Code)
	}
}

func TestWebhookHandler_PRReview(t *testing.T) {
	creator := &mockCreator{allowed: true}
	secret := "pr-secret"
	handler := NewHandler(secret, "claude-town", creator)

	payload := map[string]interface{}{
		"action": "submitted",
		"review": map[string]interface{}{
			"body": "@claude-town please address this",
		},
		"pull_request": map[string]interface{}{
			"number": 99,
			"head": map[string]interface{}{
				"ref": "feature/my-branch",
			},
		},
		"repository": map[string]interface{}{
			"owner": map[string]interface{}{
				"login": "orgname",
			},
			"name": "reponame",
		},
	}
	body, _ := json.Marshal(payload)
	sig := computeSignature(body, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request_review")
	req.Header.Set("X-Hub-Signature-256", sig)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, rr.Code)
	}

	if !creator.createCalled {
		t.Fatal("expected CreateTask to be called")
	}
	if creator.lastRequest.TaskType != "PRReviewFix" {
		t.Errorf("expected task type %q, got %q", "PRReviewFix", creator.lastRequest.TaskType)
	}
	if creator.lastRequest.Branch != "feature/my-branch" {
		t.Errorf("expected branch %q, got %q", "feature/my-branch", creator.lastRequest.Branch)
	}
	if creator.lastRequest.PullRequest != 99 {
		t.Errorf("expected PR %d, got %d", 99, creator.lastRequest.PullRequest)
	}
}

func TestWebhookHandler_PRReviewComment(t *testing.T) {
	creator := &mockCreator{allowed: true}
	secret := "rc-secret"
	handler := NewHandler(secret, "claude-town", creator)

	payload := map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"body": "@claude-town fix the lint error",
		},
		"pull_request": map[string]interface{}{
			"number": 55,
			"head": map[string]interface{}{
				"ref": "fix/lint",
			},
		},
		"repository": map[string]interface{}{
			"owner": map[string]interface{}{
				"login": "owner1",
			},
			"name": "repo1",
		},
	}
	body, _ := json.Marshal(payload)
	sig := computeSignature(body, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request_review_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, rr.Code)
	}

	if !creator.createCalled {
		t.Fatal("expected CreateTask to be called")
	}
	if creator.lastRequest.TaskType != "PRReviewFix" {
		t.Errorf("expected task type %q, got %q", "PRReviewFix", creator.lastRequest.TaskType)
	}
	if creator.lastRequest.Branch != "fix/lint" {
		t.Errorf("expected branch %q, got %q", "fix/lint", creator.lastRequest.Branch)
	}
	if creator.lastRequest.PullRequest != 55 {
		t.Errorf("expected PR %d, got %d", 55, creator.lastRequest.PullRequest)
	}
}

func TestValidateSignature(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		secret    string
		signature string
		want      bool
	}{
		{
			name:      "valid signature",
			body:      []byte("hello"),
			secret:    "secret",
			signature: computeSignature([]byte("hello"), "secret"),
			want:      true,
		},
		{
			name:      "invalid signature",
			body:      []byte("hello"),
			secret:    "secret",
			signature: "sha256=0000000000000000000000000000000000000000000000000000000000000000",
			want:      false,
		},
		{
			name:      "empty secret skips validation",
			body:      []byte("hello"),
			secret:    "",
			signature: "",
			want:      true,
		},
		{
			name:      "missing signature with non-empty secret",
			body:      []byte("hello"),
			secret:    "secret",
			signature: "",
			want:      false,
		},
		{
			name:      "wrong prefix",
			body:      []byte("hello"),
			secret:    "secret",
			signature: "sha1=abc",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateSignature(tt.body, tt.signature, tt.secret)
			if got != tt.want {
				t.Errorf("validateSignature() = %v, want %v", got, tt.want)
			}
		})
	}
}
