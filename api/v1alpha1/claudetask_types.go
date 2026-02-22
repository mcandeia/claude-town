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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClaudeTaskType defines the type of task
// +kubebuilder:validation:Enum=IssueSolve;PRReviewFix
type ClaudeTaskType string

const (
	ClaudeTaskTypeIssueSolve  ClaudeTaskType = "IssueSolve"
	ClaudeTaskTypePRReviewFix ClaudeTaskType = "PRReviewFix"
)

// ClaudeTaskPhase defines the phase of a task
type ClaudeTaskPhase string

const (
	ClaudeTaskPhasePending                 ClaudeTaskPhase = "Pending"
	ClaudeTaskPhaseRunning                 ClaudeTaskPhase = "Running"
	ClaudeTaskPhaseWaitingForClarification ClaudeTaskPhase = "WaitingForClarification"
	ClaudeTaskPhaseCompleted               ClaudeTaskPhase = "Completed"
	ClaudeTaskPhaseFailed                  ClaudeTaskPhase = "Failed"
)

// ClarificationExchange records a single clarification Q&A round.
type ClarificationExchange struct {
	Question string `json:"question"`
	Answer   string `json:"answer,omitempty"`
}

// ClaudeTaskSpec defines the desired state of ClaudeTask.
type ClaudeTaskSpec struct {
	// Repository is the full repo name (owner/repo)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`
	Repository string `json:"repository"`

	// Issue is the GitHub issue number (0 when not associated with an issue)
	// +optional
	// +kubebuilder:validation:Minimum=0
	Issue int `json:"issue,omitempty"`

	// PullRequest is the PR number (non-zero when fixing PR review)
	// +optional
	PullRequest int `json:"pullRequest,omitempty"`

	// TaskType specifies what kind of task this is
	// +kubebuilder:default=IssueSolve
	TaskType ClaudeTaskType `json:"taskType,omitempty"`

	// Branch is the git branch to work on (set for PRReviewFix)
	// +optional
	Branch string `json:"branch,omitempty"`

	// Prompt is an optional override for the Claude prompt
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// ReviewCommentID is the GitHub PR review comment ID to reply to
	// +optional
	ReviewCommentID int64 `json:"reviewCommentId,omitempty"`

	// MaxIterations is the maximum Claude iteration loops
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=50
	MaxIterations int `json:"maxIterations,omitempty"`

	// MaxClarifications is the maximum number of clarification rounds allowed.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxClarifications int `json:"maxClarifications,omitempty"`
}

// CostReport contains token usage and cost information
type CostReport struct {
	// InputTokens is the total input tokens used
	InputTokens int64 `json:"inputTokens,omitempty"`

	// OutputTokens is the total output tokens used
	OutputTokens int64 `json:"outputTokens,omitempty"`

	// CacheReadInputTokens is the total cache read input tokens used
	CacheReadInputTokens int64 `json:"cacheReadInputTokens,omitempty"`

	// CacheCreationInputTokens is the total cache creation input tokens used
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens,omitempty"`

	// EstimatedCost is the estimated cost in USD
	EstimatedCost string `json:"estimatedCost,omitempty"`

	// DurationMs is the total execution duration in milliseconds
	DurationMs int64 `json:"durationMs,omitempty"`
}

// ClaudeTaskStatus defines the observed state of ClaudeTask.
type ClaudeTaskStatus struct {
	// Phase is the current phase of the task
	Phase ClaudeTaskPhase `json:"phase,omitempty"`

	// SandboxClaimName is the name of the SandboxClaim created for this task
	SandboxClaimName string `json:"sandboxClaimName,omitempty"`

	// PodName is the name of the sandbox pod
	PodName string `json:"podName,omitempty"`

	// PodIP is the IP address of the sandbox pod
	PodIP string `json:"podIP,omitempty"`

	// StartTime is when the task started running
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the task completed
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// PullRequestURL is the URL of the created PR
	PullRequestURL string `json:"pullRequestURL,omitempty"`

	// CostReport contains token usage and cost info
	CostReport *CostReport `json:"costReport,omitempty"`

	// Clarifications records the clarification Q&A exchanges.
	// +optional
	Clarifications []ClarificationExchange `json:"clarifications,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="Issue",type=integer,JSONPath=`.spec.issue`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.taskType`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=`.status.pullRequestURL`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ClaudeTask is the Schema for the claudetasks API.
type ClaudeTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClaudeTaskSpec   `json:"spec,omitempty"`
	Status ClaudeTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClaudeTaskList contains a list of ClaudeTask.
type ClaudeTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClaudeTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClaudeTask{}, &ClaudeTaskList{})
}
