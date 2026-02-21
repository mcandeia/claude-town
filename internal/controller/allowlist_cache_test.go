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
	"testing"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
)

func TestAllowlistCache_SetAndGet(t *testing.T) {
	cache := NewAllowlistCache()
	repo := &claudetownv1alpha1.ClaudeRepository{
		Spec: claudetownv1alpha1.ClaudeRepositorySpec{
			Owner:              "org",
			Repo:               "repo",
			MaxConcurrentTasks: 3,
		},
	}
	cache.Set("org/repo", repo)

	if !cache.IsAllowed("org/repo") {
		t.Error("expected org/repo to be allowed")
	}
	if cache.IsAllowed("org/other") {
		t.Error("expected org/other to not be allowed")
	}

	got := cache.Get("org/repo")
	if got == nil {
		t.Fatal("expected repo to exist in cache")
	}
	if got.Spec.MaxConcurrentTasks != 3 {
		t.Errorf("expected max 3, got %d", got.Spec.MaxConcurrentTasks)
	}
}

func TestAllowlistCache_Delete(t *testing.T) {
	cache := NewAllowlistCache()
	repo := &claudetownv1alpha1.ClaudeRepository{
		Spec: claudetownv1alpha1.ClaudeRepositorySpec{
			Owner: "org",
			Repo:  "repo",
		},
	}
	cache.Set("org/repo", repo)
	cache.Delete("org/repo")

	if cache.IsAllowed("org/repo") {
		t.Error("expected org/repo to be removed")
	}
	if cache.Get("org/repo") != nil {
		t.Error("expected Get to return nil after delete")
	}
}
