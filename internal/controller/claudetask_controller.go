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
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
	ghclient "github.com/marcoscandeia/claude-town/internal/github"
	"github.com/marcoscandeia/claude-town/internal/sandbox"
)

const (
	// Default timeout for waiting for sandbox to become ready.
	sandboxReadyTimeout = 5 * time.Minute

	// Default timeout for Claude task execution.
	taskExecutionTimeout = 30 * time.Minute

	// Container name inside the sandbox pod (must match sandbox-template).
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

	// RestConfig is the Kubernetes REST config for exec operations.
	RestConfig *rest.Config
	// SandboxClient manages SandboxClaim resources.
	SandboxClient *sandbox.Client
	// GitHubClient is used for commenting on issues and getting tokens.
	GitHubClient *ghclient.Client
	// Allowlist provides the list of allowed repositories.
	Allowlist *AllowlistCache
	// SandboxTemplateName is the name of the SandboxTemplate to use.
	SandboxTemplateName string
	// SandboxNamespace is the namespace where sandbox pods run.
	SandboxNamespace string
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
	startComment := fmt.Sprintf("Claude is starting to work on this. A sandbox has been allocated.\n\nTask: `%s`", task.Name)
	if err := r.commentOnTask(ctx, task, owner, repo, startComment); err != nil {
		logger.Error(err, "failed to comment on task")
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

		task.Status.PodName = readyResult.PodName
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating task status with pod info: %w", err)
		}

		logger.Info("sandbox ready", "podName", readyResult.PodName)
	}

	// Get GitHub installation token for git operations.
	token, err := r.GitHubClient.GetInstallationToken(ctx)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("failed to get installation token: %v", err))
	}

	// Fetch full thread context from GitHub (non-fatal on error).
	threadContext := r.fetchThreadContext(ctx, task, owner, repo)

	// Build the command sequence.
	prompt := r.buildPrompt(task, threadContext)
	command := r.buildCommand(owner, repo, token, task, prompt)

	logger.Info("executing Claude command via exec", "repository", task.Spec.Repository, "pod", task.Status.PodName)

	// Execute command via kubectl exec and wait for completion.
	execCtx, execCancel := context.WithTimeout(ctx, taskExecutionTimeout)
	defer execCancel()

	output, err := r.execInPod(execCtx, task.Status.PodName, command)
	if err != nil {
		if strings.Contains(output, markerFailed) {
			return r.failTask(ctx, task, "Claude reported task failure")
		}
		return r.failTask(ctx, task, fmt.Sprintf("task execution error: %v", err))
	}

	// Check for failure marker in output even if exec succeeded.
	if strings.Contains(output, markerFailed) && !strings.Contains(output, markerComplete) {
		return r.failTask(ctx, task, "Claude reported task failure")
	}

	// Parse PR URL from output.
	prURL := parsePRURL(output)

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
	completionComment := "Claude has completed the task."
	if prURL != "" {
		completionComment += fmt.Sprintf("\n\nPull Request: %s", prURL)
	}
	if err := r.commentOnTask(ctx, task, owner, repo, completionComment); err != nil {
		logger.Error(err, "failed to comment completion")
	}

	// Cleanup sandbox.
	if err := r.SandboxClient.DeleteClaim(ctx, claimName); err != nil {
		logger.Error(err, "failed to cleanup SandboxClaim", "claimName", claimName)
	}

	logger.Info("task completed", "prURL", prURL)
	return ctrl.Result{}, nil
}

// execInPod executes a command in the sandbox pod via kubectl exec.
func (r *ClaudeTaskReconciler) execInPod(ctx context.Context, podName, command string) (string, error) {
	clientset, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return "", fmt.Errorf("creating clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(r.SandboxNamespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: sandboxContainerName,
			Command:   []string{"/bin/bash", "-c", command},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	output := stdout.String()
	if err != nil {
		return output, fmt.Errorf("exec failed (stderr: %s): %w", stderr.String(), err)
	}

	return output, nil
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
		failComment := fmt.Sprintf("Claude failed to complete the task.\n\nReason: %s", reason)
		if err := r.commentOnTask(ctx, task, parts[0], parts[1], failComment); err != nil {
			logger.Error(err, "failed to comment failure")
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

// commentOnTask posts a comment on the appropriate GitHub thread. If the task
// has a ReviewCommentID, it replies in the review comment thread. Otherwise, it
// posts a top-level issue/PR comment.
func (r *ClaudeTaskReconciler) commentOnTask(ctx context.Context, task *claudetownv1alpha1.ClaudeTask, owner, repo, body string) error {
	if task.Spec.ReviewCommentID > 0 && task.Spec.PullRequest > 0 {
		return r.GitHubClient.ReplyToReviewComment(ctx, owner, repo, task.Spec.PullRequest, task.Spec.ReviewCommentID, body)
	}

	issueNum := task.Spec.Issue
	if task.Spec.PullRequest > 0 {
		issueNum = task.Spec.PullRequest
	}
	return r.GitHubClient.CommentOnIssue(ctx, owner, repo, issueNum, body)
}

// fetchThreadContext fetches full GitHub thread context for the task.
// Errors are non-fatal — an empty string is returned on failure.
func (r *ClaudeTaskReconciler) fetchThreadContext(ctx context.Context, task *claudetownv1alpha1.ClaudeTask, owner, repo string) string {
	logger := logf.FromContext(ctx)
	var parts []string

	switch task.Spec.TaskType {
	case claudetownv1alpha1.ClaudeTaskTypePRReviewFix:
		if task.Spec.PullRequest > 0 {
			prCtx, err := r.GitHubClient.FetchPRContext(ctx, owner, repo, task.Spec.PullRequest)
			if err != nil {
				logger.Error(err, "failed to fetch PR context")
			} else if prCtx != "" {
				parts = append(parts, prCtx)
			}
		}
		if task.Spec.ReviewCommentID > 0 && task.Spec.PullRequest > 0 {
			threadCtx, err := r.GitHubClient.FetchReviewCommentThread(ctx, owner, repo, task.Spec.PullRequest, task.Spec.ReviewCommentID)
			if err != nil {
				logger.Error(err, "failed to fetch review comment thread")
			} else if threadCtx != "" {
				parts = append(parts, threadCtx)
			}
		}
	default:
		if task.Spec.Issue > 0 {
			issueCtx, err := r.GitHubClient.FetchIssueThread(ctx, owner, repo, task.Spec.Issue)
			if err != nil {
				logger.Error(err, "failed to fetch issue thread")
			} else if issueCtx != "" {
				parts = append(parts, issueCtx)
			}
		}
	}

	return strings.Join(parts, "\n")
}

// buildPrompt constructs the prompt for Claude based on the task type.
func (r *ClaudeTaskReconciler) buildPrompt(task *claudetownv1alpha1.ClaudeTask, threadContext string) string {
	var sb strings.Builder

	if threadContext != "" {
		sb.WriteString("# Context\n\n")
		sb.WriteString(threadContext)
		sb.WriteString("\n---\n\n# Task\n\n")
	}

	if task.Spec.Prompt != "" {
		sb.WriteString(task.Spec.Prompt)
		return sb.String()
	}

	switch task.Spec.TaskType {
	case claudetownv1alpha1.ClaudeTaskTypePRReviewFix:
		fmt.Fprintf(&sb, "Fix the review comments on PR #%d in repository %s. The branch is %s.",
			task.Spec.PullRequest, task.Spec.Repository, task.Spec.Branch)
	default:
		fmt.Fprintf(&sb, "Solve issue #%d in repository %s.",
			task.Spec.Issue, task.Spec.Repository)
	}

	return sb.String()
}

// buildCommand constructs the full shell command to set up the environment
// and run Claude inside the sandbox.
func (r *ClaudeTaskReconciler) buildCommand(owner, repo, token string, task *claudetownv1alpha1.ClaudeTask, prompt string) string {
	var sb strings.Builder

	// Set environment variables.
	// GITHUB_TOKEN is sufficient for gh CLI — no explicit auth login needed.
	fmt.Fprintf(&sb, "export GITHUB_TOKEN='%s'\n", token)
	fmt.Fprintf(&sb, "export ANTHROPIC_API_KEY='%s'\n", r.AnthropicAPIKey)

	// Clone the repository.
	cloneURL := fmt.Sprintf("https://x-access-token:${GITHUB_TOKEN}@github.com/%s/%s.git", owner, repo)
	fmt.Fprintf(&sb, "git clone %s repo\n", cloneURL)
	sb.WriteString("cd repo\n")

	// For PR review fixes, checkout the PR branch.
	if task.Spec.TaskType == claudetownv1alpha1.ClaudeTaskTypePRReviewFix && task.Spec.Branch != "" {
		fmt.Fprintf(&sb, "git checkout %s\n", task.Spec.Branch)
	}

	// Run Claude with the prompt. Use --print and --dangerously-skip-permissions
	// for autonomous execution.
	escapedPrompt := strings.ReplaceAll(prompt, "'", "'\\''")
	fmt.Fprintf(&sb, "claude --print --dangerously-skip-permissions '%s'\n", escapedPrompt)

	return sb.String()
}

// parsePRURL extracts a pull request URL from output that contains the
// ":::PR_URL:::" marker.
func parsePRURL(output string) string {
	const marker = ":::PR_URL:::"
	idx := strings.Index(output, marker)
	if idx == -1 {
		return ""
	}

	rest := output[idx+len(marker):]
	rest = strings.TrimSpace(rest)

	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaudeTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&claudetownv1alpha1.ClaudeTask{}).
		Named("claudetask").
		Complete(r)
}
