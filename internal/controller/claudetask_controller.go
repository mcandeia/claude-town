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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
	ghclient "github.com/marcoscandeia/claude-town/internal/github"
	"github.com/marcoscandeia/claude-town/internal/pty"
	"github.com/marcoscandeia/claude-town/internal/sandbox"
)

const (
	// Default timeout for waiting for sandbox to become ready.
	sandboxReadyTimeout = 5 * time.Minute

	// Default timeout for Claude task execution.
	taskExecutionTimeout = 30 * time.Minute

	// Name of the container inside sandbox pods.
	sandboxContainerName = "claude"

	// Completion markers output by Claude.
	markerComplete = ":::TASK_COMPLETE:::"
	markerFailed   = ":::TASK_FAILED:::"
)

// ClaudeTaskReconciler reconciles a ClaudeTask object.
// It orchestrates the full lifecycle: creating sandboxes, executing commands
// via kubectl exec, running Claude, parsing results, and cleaning up.
type ClaudeTaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SandboxClient manages SandboxClaim resources.
	SandboxClient *sandbox.Client
	// GitHubClient is used for commenting on issues and getting tokens.
	GitHubClient *ghclient.Client
	// ExecClient runs commands in sandbox pods via the K8s exec API.
	ExecClient *pty.ExecClient
	// Allowlist provides the list of allowed repositories.
	Allowlist *AllowlistCache
	// SandboxTemplateName is the name of the SandboxTemplate to use.
	SandboxTemplateName string
	// AnthropicAPIKey is the Anthropic API key for Claude.
	AnthropicAPIKey string
}

// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=claudetasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=claudetasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=claudetasks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

func (r *ClaudeTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var task claudetownv1alpha1.ClaudeTask
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("reconciling ClaudeTask", "phase", task.Status.Phase, "repository", task.Spec.Repository, "issue", task.Spec.Issue)

	switch task.Status.Phase {
	case "", claudetownv1alpha1.ClaudeTaskPhasePending:
		return r.reconcilePending(ctx, &task)
	case claudetownv1alpha1.ClaudeTaskPhaseRunning:
		return r.reconcileRunning(ctx, &task)
	case claudetownv1alpha1.ClaudeTaskPhaseCompleted, claudetownv1alpha1.ClaudeTaskPhaseFailed:
		// Terminal states — nothing to do.
		return ctrl.Result{}, nil
	default:
		logger.Info("unknown phase", "phase", task.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// reconcilePending handles the Pending phase: creates a SandboxClaim and
// transitions to Running.
func (r *ClaudeTaskReconciler) reconcilePending(ctx context.Context, task *claudetownv1alpha1.ClaudeTask) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	parts := strings.SplitN(task.Spec.Repository, "/", 2)
	if len(parts) != 2 {
		return r.failTask(ctx, task, fmt.Sprintf("invalid repository format: %s", task.Spec.Repository))
	}
	owner, repo := parts[0], parts[1]

	// Generate deterministic claim name.
	claimName := sandbox.ClaimName(owner, repo, task.Spec.Issue)

	logger.Info("creating SandboxClaim", "claimName", claimName)

	if err := r.SandboxClient.CreateClaim(ctx, claimName, r.SandboxTemplateName); err != nil {
		if !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("creating SandboxClaim: %w", err)
		}
		logger.Info("SandboxClaim already exists", "claimName", claimName)
	}

	// Update status to Running.
	now := metav1.Now()
	task.Status.Phase = claudetownv1alpha1.ClaudeTaskPhaseRunning
	task.Status.SandboxClaimName = claimName
	task.Status.StartTime = &now
	if err := r.Status().Update(ctx, task); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating task status to Running: %w", err)
	}

	// Comment on GitHub that work is starting.
	issueNum := task.Spec.Issue
	if task.Spec.PullRequest > 0 {
		issueNum = task.Spec.PullRequest
	}
	startComment := fmt.Sprintf("Claude is starting to work on this. A sandbox has been allocated.\n\nTask: `%s`", task.Name)
	if err := r.GitHubClient.CommentOnIssue(ctx, owner, repo, issueNum, startComment); err != nil {
		logger.Error(err, "failed to comment on issue", "owner", owner, "repo", repo, "issue", issueNum)
		// Non-fatal — continue even if commenting fails.
	}

	// Requeue immediately to proceed to Running phase.
	return ctrl.Result{Requeue: true}, nil
}

// reconcileRunning handles the Running phase: waits for sandbox, executes
// Claude via kubectl exec, and transitions to Completed or Failed.
func (r *ClaudeTaskReconciler) reconcileRunning(ctx context.Context, task *claudetownv1alpha1.ClaudeTask) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	claimName := task.Status.SandboxClaimName
	if claimName == "" {
		return r.failTask(ctx, task, "no SandboxClaim name in status")
	}

	parts := strings.SplitN(task.Spec.Repository, "/", 2)
	if len(parts) != 2 {
		return r.failTask(ctx, task, fmt.Sprintf("invalid repository format: %s", task.Spec.Repository))
	}
	owner, repo := parts[0], parts[1]

	// If we don't have a pod name yet, wait for sandbox to be ready.
	if task.Status.PodName == "" {
		logger.Info("waiting for sandbox to become ready", "claimName", claimName)

		readyResult, err := r.SandboxClient.WaitForReady(ctx, claimName, sandboxReadyTimeout)
		if err != nil {
			return r.failTask(ctx, task, fmt.Sprintf("sandbox failed to become ready: %v", err))
		}

		podIP, err := r.SandboxClient.GetPodIP(ctx, readyResult.PodName)
		if err != nil {
			return r.failTask(ctx, task, fmt.Sprintf("failed to get pod IP: %v", err))
		}

		task.Status.PodName = readyResult.PodName
		task.Status.PodIP = podIP
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating task status with pod info: %w", err)
		}

		logger.Info("sandbox ready", "podName", readyResult.PodName, "podIP", podIP)
	}

	// Get GitHub installation token for git operations.
	token, err := r.GitHubClient.GetInstallationToken(ctx)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("failed to get installation token: %v", err))
	}

	// Build the command script.
	prompt := r.buildPrompt(task)
	script := r.buildCommand(owner, repo, token, task, prompt)

	logger.Info("executing Claude command via exec", "repository", task.Spec.Repository, "podName", task.Status.PodName)

	// Execute command and wait for completion marker.
	execCtx, execCancel := context.WithTimeout(ctx, taskExecutionTimeout)
	defer execCancel()

	output, err := r.ExecClient.RunAndWaitForMarker(execCtx, task.Status.PodName, sandboxContainerName, script, markerComplete)

	// Log output for debugging (truncate to last 2000 chars).
	logger.Info("claude exec output", "output_tail", truncateTail(output, 2000))

	if err != nil {
		// Check if the task failed explicitly.
		if strings.Contains(output, markerFailed) {
			return r.failTask(ctx, task, "Claude reported task failure")
		}
		logger.Info("task execution failed", "error", err)
		return r.failTask(ctx, task, fmt.Sprintf("task execution error: %v", err))
	}

	// Parse PR URL from output.
	prURL := pty.ParsePRURL(output)

	// Update status to Completed.
	now := metav1.Now()
	task.Status.Phase = claudetownv1alpha1.ClaudeTaskPhaseCompleted
	task.Status.CompletionTime = &now
	if prURL != "" {
		task.Status.PullRequestURL = prURL
	}
	if err := r.Status().Update(ctx, task); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating task status to Completed: %w", err)
	}

	// Comment completion on GitHub.
	issueNum := task.Spec.Issue
	if task.Spec.PullRequest > 0 {
		issueNum = task.Spec.PullRequest
	}
	completionComment := "Claude has completed the task."
	if prURL != "" {
		completionComment += fmt.Sprintf("\n\nPull Request: %s", prURL)
	}
	if err := r.GitHubClient.CommentOnIssue(ctx, owner, repo, issueNum, completionComment); err != nil {
		logger.Error(err, "failed to comment completion on issue")
	}

	// Cleanup sandbox.
	if err := r.SandboxClient.DeleteClaim(ctx, claimName); err != nil {
		logger.Error(err, "failed to cleanup SandboxClaim", "claimName", claimName)
	}

	logger.Info("task completed", "prURL", prURL)
	return ctrl.Result{}, nil
}

// failTask transitions the task to Failed, comments on GitHub, and cleans up.
func (r *ClaudeTaskReconciler) failTask(ctx context.Context, task *claudetownv1alpha1.ClaudeTask, reason string) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("failing task", "reason", reason)

	now := metav1.Now()
	task.Status.Phase = claudetownv1alpha1.ClaudeTaskPhaseFailed
	task.Status.CompletionTime = &now
	if err := r.Status().Update(ctx, task); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating task status to Failed: %w", err)
	}

	// Comment failure on GitHub.
	parts := strings.SplitN(task.Spec.Repository, "/", 2)
	if len(parts) == 2 {
		issueNum := task.Spec.Issue
		if task.Spec.PullRequest > 0 {
			issueNum = task.Spec.PullRequest
		}
		failComment := fmt.Sprintf("Claude failed to complete the task.\n\nReason: %s", reason)
		if err := r.GitHubClient.CommentOnIssue(ctx, parts[0], parts[1], issueNum, failComment); err != nil {
			logger.Error(err, "failed to comment failure on issue")
		}
	}

	// Cleanup sandbox.
	if task.Status.SandboxClaimName != "" {
		if err := r.SandboxClient.DeleteClaim(ctx, task.Status.SandboxClaimName); err != nil {
			logger.Error(err, "failed to cleanup SandboxClaim")
		}
	}

	return ctrl.Result{}, nil
}

// buildPrompt constructs the prompt for Claude based on the task type.
func (r *ClaudeTaskReconciler) buildPrompt(task *claudetownv1alpha1.ClaudeTask) string {
	if task.Spec.Prompt != "" {
		return task.Spec.Prompt
	}

	switch task.Spec.TaskType {
	case claudetownv1alpha1.ClaudeTaskTypePRReviewFix:
		return fmt.Sprintf("Fix the review comments on PR #%d in repository %s. The branch is %s. Follow the fix-pr-review skill instructions.",
			task.Spec.PullRequest, task.Spec.Repository, task.Spec.Branch)
	default:
		return fmt.Sprintf("Solve issue #%d in repository %s. Follow the solve-issue skill instructions.",
			task.Spec.Issue, task.Spec.Repository)
	}
}

// buildCommand constructs the full shell script to set up the environment
// and run Claude inside the sandbox.
func (r *ClaudeTaskReconciler) buildCommand(owner, repo, token string, task *claudetownv1alpha1.ClaudeTask, prompt string) string {
	var sb strings.Builder

	// Fail fast on setup errors.
	sb.WriteString("set -e\n")

	// Set environment variables.
	sb.WriteString(fmt.Sprintf("export GITHUB_TOKEN='%s'\n", token))
	sb.WriteString(fmt.Sprintf("export ANTHROPIC_API_KEY='%s'\n", r.AnthropicAPIKey))

	// gh CLI auto-detects GITHUB_TOKEN from the environment.

	// Clone the repository into a clean directory.
	cloneURL := fmt.Sprintf("https://x-access-token:${GITHUB_TOKEN}@github.com/%s/%s.git", owner, repo)
	sb.WriteString(fmt.Sprintf("rm -rf /home/node/workspace/repo && git clone %s /home/node/workspace/repo\n", cloneURL))
	sb.WriteString("cd /home/node/workspace/repo\n")

	// For PR review fixes, checkout the PR branch.
	if task.Spec.TaskType == claudetownv1alpha1.ClaudeTaskTypePRReviewFix && task.Spec.Branch != "" {
		sb.WriteString(fmt.Sprintf("git checkout %s\n", task.Spec.Branch))
	}

	// Disable set -e so we can capture Claude's exit code.
	sb.WriteString("set +e\n")

	// Run Claude in non-interactive agentic mode. --print is required for
	// headless (no TTY) execution. --dangerously-skip-permissions auto-approves
	// all tool use (file edits, bash commands, etc).
	escapedPrompt := strings.ReplaceAll(prompt, "'", "'\\''")
	sb.WriteString(fmt.Sprintf("claude --print --dangerously-skip-permissions '%s' 2>&1\n",
		escapedPrompt))

	// Emit completion markers based on exit code.
	sb.WriteString("CLAUDE_EXIT=$?\n")
	sb.WriteString("if [ $CLAUDE_EXIT -eq 0 ]; then\n")
	sb.WriteString(fmt.Sprintf("  echo '%s'\n", markerComplete))
	sb.WriteString("else\n")
	sb.WriteString(fmt.Sprintf("  echo '%s'\n", markerFailed))
	sb.WriteString("  exit $CLAUDE_EXIT\n")
	sb.WriteString("fi\n")

	return sb.String()
}

// truncateTail returns the last n bytes of s, or s if shorter.
func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaudeTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&claudetownv1alpha1.ClaudeTask{}).
		Named("claudetask").
		Complete(r)
}
