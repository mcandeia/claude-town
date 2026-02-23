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
	"encoding/json"
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
	markerComplete      = ":::TASK_COMPLETE:::"
	markerFailed        = ":::TASK_FAILED:::"
	markerClarification = ":::CLARIFICATION:::"

	// Default max clarification rounds when spec value is 0.
	defaultMaxClarifications = 3

	// How long to wait for a user's clarification reply before timing out.
	clarificationWaitTimeout = 24 * time.Hour

	// streamFormatterScript is an inline Node.js script that reads stream-json
	// lines from stdin and writes a human-readable log to stdout.
	streamFormatterScript = `
const rl = require("readline").createInterface({ input: process.stdin });
rl.on("line", (line) => {
  let e;
  try { e = JSON.parse(line); } catch { process.stdout.write(line + "\\n"); return; }
  const t = e.type;
  if (t === "system") {
    console.log("[init] model=" + e.model + " session=" + e.session_id);
  } else if (t === "assistant") {
    const c = (e.message && e.message.content) || [];
    for (const b of c) {
      if (b.type === "thinking") console.log("[thinking] " + b.thinking);
      else if (b.type === "tool_use") console.log("[tool] " + b.name + ": " + JSON.stringify(b.input));
      else if (b.type === "text") console.log("[text] " + b.text);
    }
  } else if (t === "user" && e.tool_use_result) {
    const r = e.tool_use_result;
    if (r.stdout) console.log("[result] " + r.stdout.slice(0, 500));
    if (r.stderr) console.log("[stderr] " + r.stderr.slice(0, 500));
  } else if (t === "result") {
    console.log("[done] " + e.subtype + " | cost=$" + (e.total_cost_usd||0).toFixed(4) + " | turns=" + e.num_turns + " | " + ((e.duration_ms||0)/1000).toFixed(1) + "s");
  }
});
`
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
	GitHubClient ghclient.GitHubClient
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
	case claudetownv1alpha1.ClaudeTaskPhaseWaitingForClarification:
		return r.reconcileWaitingForClarification(ctx, &task)
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
	ghClient := r.resolveGitHubClient(task.Spec.Repository)
	startComment := fmt.Sprintf("Claude is starting to work on this. A sandbox has been allocated.\n\nTask: `%s`", task.Name)
	if err := r.commentOnTask(ctx, ghClient, task, owner, repo, startComment); err != nil {
		logger.Error(err, "failed to comment on task")
		// Non-fatal — continue even if commenting fails.
	}

	// Requeue immediately to proceed to Running phase.
	return ctrl.Result{Requeue: true}, nil
}

// reconcileWaitingForClarification handles the WaitingForClarification phase:
// checks timeout (24h) and requeues periodically until the webhook resumes the task.
func (r *ClaudeTaskReconciler) reconcileWaitingForClarification(ctx context.Context, task *claudetownv1alpha1.ClaudeTask) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	if task.Status.StartTime != nil {
		elapsed := time.Since(task.Status.StartTime.Time)
		if elapsed > clarificationWaitTimeout {
			return r.failTask(ctx, task, "clarification wait timed out after 24h")
		}
	}

	logger.Info("waiting for clarification response", "clarifications", len(task.Status.Clarifications))
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
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

	// Detect if this is a resume after clarification.
	isResume := false
	if n := len(task.Status.Clarifications); n > 0 && task.Status.Clarifications[n-1].Answer != "" {
		isResume = true
		logger.Info("resuming after clarification", "round", n)
	}

	// Resolve GitHub client for this repo (per-repo PAT > global).
	ghClient := r.resolveGitHubClient(task.Spec.Repository)

	// Get clone token for git operations.
	token, err := ghClient.GetCloneToken(ctx)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("failed to get clone token: %v", err))
	}

	// Fetch full thread context from GitHub (non-fatal on error).
	threadContext := r.fetchThreadContext(ctx, ghClient, task, owner, repo)

	// Build the command sequence.
	prompt := r.buildPrompt(task, threadContext)
	command := r.buildCommand(owner, repo, token, task, prompt, isResume)

	logger.Info("executing Claude command via exec", "repository", task.Spec.Repository, "pod", task.Status.PodName, "resume", isResume)

	// Execute command via kubectl exec and wait for completion.
	execCtx, execCancel := context.WithTimeout(ctx, taskExecutionTimeout)
	defer execCancel()

	output, err := r.execInPod(execCtx, task.Status.PodName, command)

	// Parse JSON output from Claude CLI.
	cr, resultText := parseClaudeOutput(output)
	task.Status.CostReport = accumulateCost(task.Status.CostReport, cr)

	// Check for clarification marker BEFORE failure/completion markers.
	if question := parseClarification(resultText); question != "" {
		// Save accumulated cost before transitioning to clarification.
		if updateErr := r.Status().Update(ctx, task); updateErr != nil {
			logger.Error(updateErr, "failed to save cost report before clarification")
		}
		return r.handleClarification(ctx, ghClient, task, owner, repo, question)
	}

	if err != nil {
		if strings.Contains(resultText, markerFailed) {
			return r.failTask(ctx, task, "Claude reported task failure")
		}
		return r.failTask(ctx, task, fmt.Sprintf("task execution error: %v", err))
	}

	// Check for failure marker in output even if exec succeeded.
	if strings.Contains(resultText, markerFailed) && !strings.Contains(resultText, markerComplete) {
		return r.failTask(ctx, task, "Claude reported task failure")
	}

	// Parse PR URL from output.
	prURL := parsePRURL(resultText)

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
	completionComment += formatCostReport(task.Status.CostReport)
	if err := r.commentOnTask(ctx, ghClient, task, owner, repo, completionComment); err != nil {
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

// handleClarification transitions the task to WaitingForClarification, appends the
// question to the clarification history, and posts the question on GitHub.
func (r *ClaudeTaskReconciler) handleClarification(ctx context.Context, ghClient ghclient.GitHubClient, task *claudetownv1alpha1.ClaudeTask, owner, repo, question string) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	maxRounds := task.Spec.MaxClarifications
	if maxRounds == 0 {
		maxRounds = defaultMaxClarifications
	}
	if len(task.Status.Clarifications) >= maxRounds {
		return r.failTask(ctx, task, fmt.Sprintf("exceeded max clarification rounds (%d)", maxRounds))
	}

	task.Status.Clarifications = append(task.Status.Clarifications, claudetownv1alpha1.ClarificationExchange{
		Question: question,
	})
	task.Status.Phase = claudetownv1alpha1.ClaudeTaskPhaseWaitingForClarification
	if err := r.Status().Update(ctx, task); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating task status to WaitingForClarification: %w", err)
	}

	// Post the question on GitHub.
	botName := ""
	if ghClient != nil {
		botName = ghClient.BotName()
	}
	comment := fmt.Sprintf("**Claude needs clarification to continue:**\n\n> %s\n\nPlease reply mentioning @%s with your answer.", question, botName)
	comment += formatCostReport(task.Status.CostReport)
	if err := r.commentOnTask(ctx, ghClient, task, owner, repo, comment); err != nil {
		logger.Error(err, "failed to post clarification question on GitHub")
	}

	logger.Info("task waiting for clarification", "round", len(task.Status.Clarifications), "question", question)
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
	ghClient := r.resolveGitHubClient(task.Spec.Repository)
	parts := strings.SplitN(task.Spec.Repository, "/", 2)
	if len(parts) == 2 {
		failComment := fmt.Sprintf("Claude failed to complete the task.\n\nReason: %s", reason)
		failComment += formatCostReport(task.Status.CostReport)
		if err := r.commentOnTask(ctx, ghClient, task, parts[0], parts[1], failComment); err != nil {
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
func (r *ClaudeTaskReconciler) commentOnTask(ctx context.Context, ghClient ghclient.GitHubClient, task *claudetownv1alpha1.ClaudeTask, owner, repo, body string) error {
	if ghClient == nil {
		return nil
	}
	if task.Spec.ReviewCommentID > 0 && task.Spec.PullRequest > 0 {
		return ghClient.ReplyToReviewComment(ctx, owner, repo, task.Spec.PullRequest, task.Spec.ReviewCommentID, body)
	}

	issueNum := task.Spec.Issue
	if task.Spec.PullRequest > 0 {
		issueNum = task.Spec.PullRequest
	}
	return ghClient.CommentOnIssue(ctx, owner, repo, issueNum, body)
}

// fetchThreadContext fetches full GitHub thread context for the task.
// Errors are non-fatal — an empty string is returned on failure.
func (r *ClaudeTaskReconciler) fetchThreadContext(ctx context.Context, ghClient ghclient.GitHubClient, task *claudetownv1alpha1.ClaudeTask, owner, repo string) string {
	logger := logf.FromContext(ctx)
	if ghClient == nil {
		return ""
	}
	var parts []string

	switch task.Spec.TaskType {
	case claudetownv1alpha1.ClaudeTaskTypePRReviewFix:
		if task.Spec.PullRequest > 0 {
			prCtx, err := ghClient.FetchPRContext(ctx, owner, repo, task.Spec.PullRequest)
			if err != nil {
				logger.Error(err, "failed to fetch PR context")
			} else if prCtx != "" {
				parts = append(parts, prCtx)
			}
		}
		if task.Spec.ReviewCommentID > 0 && task.Spec.PullRequest > 0 {
			threadCtx, err := ghClient.FetchReviewCommentThread(ctx, owner, repo, task.Spec.PullRequest, task.Spec.ReviewCommentID)
			if err != nil {
				logger.Error(err, "failed to fetch review comment thread")
			} else if threadCtx != "" {
				parts = append(parts, threadCtx)
			}
		}
	default:
		if task.Spec.Issue > 0 {
			issueCtx, err := ghClient.FetchIssueThread(ctx, owner, repo, task.Spec.Issue)
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
// If there are previous clarification exchanges, they are included between
// the Context and Task sections.
func (r *ClaudeTaskReconciler) buildPrompt(task *claudetownv1alpha1.ClaudeTask, threadContext string) string {
	var sb strings.Builder

	if threadContext != "" {
		sb.WriteString("# Context\n\n")
		sb.WriteString(threadContext)
		sb.WriteString("\n\n")
	}

	// Include clarification history if present.
	if len(task.Status.Clarifications) > 0 {
		sb.WriteString("# Previous Clarification Exchanges\n\n")
		for i, c := range task.Status.Clarifications {
			fmt.Fprintf(&sb, "## Round %d\n", i+1)
			fmt.Fprintf(&sb, "**Your question:** %s\n", c.Question)
			if c.Answer != "" {
				fmt.Fprintf(&sb, "**User's answer:** %s\n", c.Answer)
			}
			sb.WriteString("\n")
		}
	}

	if threadContext != "" || len(task.Status.Clarifications) > 0 {
		sb.WriteString("---\n\n# Task\n\n")
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
// and run Claude inside the sandbox. When isResume is true the repo is
// already cloned so we cd into it and pull instead of cloning.
func (r *ClaudeTaskReconciler) buildCommand(owner, repo, token string, task *claudetownv1alpha1.ClaudeTask, prompt string, isResume bool) string {
	var sb strings.Builder

	// Set environment variables.
	// GITHUB_TOKEN is sufficient for gh CLI — no explicit auth login needed.
	fmt.Fprintf(&sb, "export GITHUB_TOKEN='%s'\n", token)
	fmt.Fprintf(&sb, "export ANTHROPIC_API_KEY='%s'\n", r.AnthropicAPIKey)

	if isResume {
		// Repo was already cloned in a previous run — just pull latest changes.
		sb.WriteString("cd repo\n")
		sb.WriteString("git pull\n")
	} else {
		// Clone the repository.
		cloneURL := fmt.Sprintf("https://x-access-token:${GITHUB_TOKEN}@github.com/%s/%s.git", owner, repo)
		fmt.Fprintf(&sb, "git clone %s repo\n", cloneURL)
		sb.WriteString("cd repo\n")
	}

	// For PR review fixes, checkout the PR branch.
	if task.Spec.TaskType == claudetownv1alpha1.ClaudeTaskTypePRReviewFix && task.Spec.Branch != "" {
		fmt.Fprintf(&sb, "git checkout %s\n", task.Spec.Branch)
	}

	// Run Claude with the prompt. Use --print and --dangerously-skip-permissions
	// for autonomous execution.
	// Pipe raw JSON to stdout (exec buffer) and a human-readable format to
	// the container's PID 1 stdout (visible via kubectl logs).
	escapedPrompt := strings.ReplaceAll(prompt, "'", "'\\''")
	fmt.Fprintf(&sb, "claude --print --verbose --output-format stream-json --dangerously-skip-permissions '%s' 2>&1 | tee >(node -e '%s' > /proc/1/fd/1)\n", escapedPrompt, streamFormatterScript)

	return sb.String()
}

// parseClarification extracts the question text after the :::CLARIFICATION::: marker.
func parseClarification(output string) string {
	idx := strings.Index(output, markerClarification)
	if idx == -1 {
		return ""
	}
	remaining := output[idx+len(markerClarification):]
	remaining = strings.TrimSpace(remaining)
	// Take everything until the next marker or end of output.
	for _, m := range []string{markerComplete, markerFailed, ":::PR_URL:::"} {
		if i := strings.Index(remaining, m); i != -1 {
			remaining = remaining[:i]
		}
	}
	return strings.TrimSpace(remaining)
}

// parsePRURL extracts a pull request URL from output that contains the
// ":::PR_URL:::" marker.
func parsePRURL(output string) string {
	const marker = ":::PR_URL:::"
	idx := strings.Index(output, marker)
	if idx == -1 {
		return ""
	}

	remaining := output[idx+len(marker):]
	remaining = strings.TrimSpace(remaining)

	fields := strings.Fields(remaining)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// claudeResult represents the JSON output from Claude CLI with --output-format json.
type claudeResult struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMs   int64   `json:"duration_ms"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// parseClaudeOutput extracts the Claude JSON result from raw exec output.
// Git clone output and other text may precede the JSON object, so we find
// the last top-level JSON object in the output. Returns the parsed struct
// and the text result for marker parsing. On failure, returns empty struct
// and the raw output as fallback.
func parseClaudeOutput(output string) (claudeResult, string) {
	// Find the last '{' that starts a top-level JSON object.
	lastBrace := strings.LastIndex(output, "{")
	if lastBrace == -1 {
		return claudeResult{}, output
	}

	// Try parsing from each '{' starting from the last one, working backwards.
	for i := lastBrace; i >= 0; i-- {
		if output[i] != '{' {
			continue
		}
		candidate := output[i:]
		var cr claudeResult
		if err := json.Unmarshal([]byte(candidate), &cr); err == nil {
			return cr, cr.Result
		}
	}

	return claudeResult{}, output
}

// accumulateCost adds the cost from a claudeResult to an existing CostReport,
// creating a new one if nil.
func accumulateCost(existing *claudetownv1alpha1.CostReport, cr claudeResult) *claudetownv1alpha1.CostReport {
	if existing == nil {
		existing = &claudetownv1alpha1.CostReport{}
	}
	existing.InputTokens += cr.Usage.InputTokens
	existing.OutputTokens += cr.Usage.OutputTokens
	existing.CacheReadInputTokens += cr.Usage.CacheReadInputTokens
	existing.CacheCreationInputTokens += cr.Usage.CacheCreationInputTokens
	existing.DurationMs += cr.DurationMs

	// Recalculate total cost: parse existing cost and add new.
	var totalCost float64
	if existing.EstimatedCost != "" {
		_, _ = fmt.Sscanf(existing.EstimatedCost, "$%f", &totalCost)
	}
	totalCost += cr.TotalCostUSD
	existing.EstimatedCost = fmt.Sprintf("$%.4f", totalCost)

	return existing
}

// formatCostReport returns a human-readable string for a CostReport.
func formatCostReport(cr *claudetownv1alpha1.CostReport) string {
	if cr == nil {
		return ""
	}
	durationSec := float64(cr.DurationMs) / 1000.0
	return fmt.Sprintf("\n\n---\n📊 **Cost:** %s | **Tokens:** %d in / %d out | **Duration:** %.1fs",
		cr.EstimatedCost, cr.InputTokens, cr.OutputTokens, durationSec)
}

// resolveGitHubClient returns the per-repo client if available, else the global client.
func (r *ClaudeTaskReconciler) resolveGitHubClient(repository string) ghclient.GitHubClient {
	if r.Allowlist != nil {
		if c := r.Allowlist.GetClient(repository); c != nil {
			return c
		}
	}
	return r.GitHubClient
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaudeTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&claudetownv1alpha1.ClaudeTask{}).
		Named("claudetask").
		Complete(r)
}
