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

func TestAllowlistCache_Pattern(t *testing.T) {
	cache := NewAllowlistCache()

	// Pattern: all repos in "my-org"
	orgWildcard := &claudetownv1alpha1.ClaudeRepository{
		Spec: claudetownv1alpha1.ClaudeRepositorySpec{
			RepositoryPattern:  "my-org/.*",
			MaxConcurrentTasks: 5,
		},
	}
	cache.Set("my-org/.*", orgWildcard)

	// Pattern: only frontend-* repos in "other-org"
	frontendPattern := &claudetownv1alpha1.ClaudeRepository{
		Spec: claudetownv1alpha1.ClaudeRepositorySpec{
			RepositoryPattern:  "other-org/frontend-.*",
			MaxConcurrentTasks: 2,
		},
	}
	cache.Set("other-org/frontend-.*", frontendPattern)

	// Should match my-org wildcard.
	if !cache.IsAllowed("my-org/repo-a") {
		t.Error("expected my-org/repo-a to be allowed by pattern")
	}
	if !cache.IsAllowed("my-org/anything") {
		t.Error("expected my-org/anything to be allowed by pattern")
	}

	// Should match other-org/frontend-* pattern.
	if !cache.IsAllowed("other-org/frontend-app") {
		t.Error("expected other-org/frontend-app to be allowed by pattern")
	}

	// Should NOT match.
	if cache.IsAllowed("other-org/backend-api") {
		t.Error("expected other-org/backend-api to NOT be allowed")
	}
	if cache.IsAllowed("unknown-org/repo") {
		t.Error("expected unknown-org/repo to NOT be allowed")
	}

	// Get should return the matching repo config.
	got := cache.Get("my-org/some-repo")
	if got == nil {
		t.Fatal("expected Get to return a match for my-org/some-repo")
	}
	if got.Spec.MaxConcurrentTasks != 5 {
		t.Errorf("expected max 5, got %d", got.Spec.MaxConcurrentTasks)
	}
}

func TestAllowlistCache_PatternDelete(t *testing.T) {
	cache := NewAllowlistCache()
	repo := &claudetownv1alpha1.ClaudeRepository{
		Spec: claudetownv1alpha1.ClaudeRepositorySpec{
			RepositoryPattern: "org/.*",
		},
	}
	cache.Set("org/.*", repo)

	if !cache.IsAllowed("org/anything") {
		t.Error("expected org/anything to be allowed")
	}

	cache.Delete("org/.*")

	if cache.IsAllowed("org/anything") {
		t.Error("expected org/anything to NOT be allowed after delete")
	}
}

func TestAllowlistCache_ExactAndPatternCoexist(t *testing.T) {
	cache := NewAllowlistCache()

	// Exact match with high concurrency.
	exact := &claudetownv1alpha1.ClaudeRepository{
		Spec: claudetownv1alpha1.ClaudeRepositorySpec{
			Owner:              "org",
			Repo:               "special-repo",
			MaxConcurrentTasks: 10,
		},
	}
	cache.Set("org/special-repo", exact)

	// Pattern for all org repos with lower concurrency.
	pattern := &claudetownv1alpha1.ClaudeRepository{
		Spec: claudetownv1alpha1.ClaudeRepositorySpec{
			RepositoryPattern:  "org/.*",
			MaxConcurrentTasks: 2,
		},
	}
	cache.Set("org/.*", pattern)

	// Exact match should take priority in Get.
	got := cache.Get("org/special-repo")
	if got == nil {
		t.Fatal("expected Get to return a match")
	}
	if got.Spec.MaxConcurrentTasks != 10 {
		t.Errorf("expected exact match (max 10), got %d", got.Spec.MaxConcurrentTasks)
	}

	// Other repos should match pattern.
	got = cache.Get("org/other-repo")
	if got == nil {
		t.Fatal("expected Get to return a match for org/other-repo")
	}
	if got.Spec.MaxConcurrentTasks != 2 {
		t.Errorf("expected pattern match (max 2), got %d", got.Spec.MaxConcurrentTasks)
	}
}
