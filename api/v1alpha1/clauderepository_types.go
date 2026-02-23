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

// SecretRef references a key in a Kubernetes Secret.
type SecretRef struct {
	// Name is the name of the Secret.
	Name string `json:"name"`
	// Key is the key within the Secret. Defaults to "token".
	// +kubebuilder:default=token
	Key string `json:"key,omitempty"`
}

// ClaudeRepositorySpec defines the desired state of ClaudeRepository.
// Use Owner+Repo for exact match, or RepositoryPattern for regex matching.
type ClaudeRepositorySpec struct {
	// Owner is the GitHub organization or user (exact match).
	// +optional
	Owner string `json:"owner,omitempty"`

	// Repo is the repository name (exact match).
	// +optional
	Repo string `json:"repo,omitempty"`

	// RepositoryPattern is a regex pattern matched against "owner/repo".
	// When set, Owner and Repo fields are ignored for matching purposes.
	// Examples: "my-org/.*", "my-org/frontend-.*", ".*/my-repo"
	// +optional
	RepositoryPattern string `json:"repositoryPattern,omitempty"`

	// Labels restricts the bot to only respond on issues with these labels.
	// Empty means all issues are allowed.
	// +optional
	Labels []string `json:"labels,omitempty"`

	// MaxConcurrentTasks is the maximum number of concurrent tasks for this repo
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	MaxConcurrentTasks int `json:"maxConcurrentTasks,omitempty"`

	// PATSecretRef references a K8s Secret containing a GitHub PAT.
	// The Secret must have a key "token" with the PAT value.
	// When set, this PAT is used instead of the global GitHub App/PAT.
	// +optional
	PATSecretRef *SecretRef `json:"patSecretRef,omitempty"`

	// AllowedUsers is a list of GitHub usernames allowed to trigger Claude.
	// Merged with global allowedUsers. Empty = no user-based restriction.
	// +optional
	AllowedUsers []string `json:"allowedUsers,omitempty"`

	// AllowedRoles is a list of repo permission levels that can trigger Claude.
	// Valid values: "admin", "maintain", "write".
	// Merged with global allowedRoles. Empty = no role-based restriction.
	// +optional
	AllowedRoles []string `json:"allowedRoles,omitempty"`
}

// ClaudeRepositoryStatus defines the observed state of ClaudeRepository.
type ClaudeRepositoryStatus struct {
	// ActiveTasks is the current number of running tasks for this repo
	ActiveTasks int `json:"activeTasks,omitempty"`

	// WebhookID is the GitHub webhook ID created for this repository.
	// +optional
	WebhookID int64 `json:"webhookID,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repo`
// +kubebuilder:printcolumn:name="Pattern",type=string,JSONPath=`.spec.repositoryPattern`
// +kubebuilder:printcolumn:name="Max Tasks",type=integer,JSONPath=`.spec.maxConcurrentTasks`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeTasks`

// ClaudeRepository is the Schema for the clauderepositories API.
type ClaudeRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClaudeRepositorySpec   `json:"spec,omitempty"`
	Status ClaudeRepositoryStatus `json:"status,omitempty"`
}

// IsPattern returns true if this entry uses a regex pattern instead of exact match.
func (r *ClaudeRepository) IsPattern() bool {
	return r.Spec.RepositoryPattern != ""
}

// FullName returns "owner/repo" for exact-match entries.
// For pattern entries it returns the pattern itself.
func (r *ClaudeRepository) FullName() string {
	if r.IsPattern() {
		return r.Spec.RepositoryPattern
	}
	return r.Spec.Owner + "/" + r.Spec.Repo
}

// +kubebuilder:object:root=true

// ClaudeRepositoryList contains a list of ClaudeRepository.
type ClaudeRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClaudeRepository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClaudeRepository{}, &ClaudeRepositoryList{})
}
