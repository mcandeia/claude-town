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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ClaudeTaskSpec defines the desired state of ClaudeTask.
type ClaudeTaskSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of ClaudeTask. Edit claudetask_types.go to remove/update
	Foo string `json:"foo,omitempty"`
}

// ClaudeTaskStatus defines the observed state of ClaudeTask.
type ClaudeTaskStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

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
