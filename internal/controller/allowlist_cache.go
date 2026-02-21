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
	"sync"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
)

// AllowlistCache is a thread-safe in-memory cache of allowed repositories.
// It is populated by the ClaudeRepositoryReconciler and read by the webhook handler.
type AllowlistCache struct {
	mu    sync.RWMutex
	repos map[string]*claudetownv1alpha1.ClaudeRepository
}

// NewAllowlistCache creates a new empty AllowlistCache.
func NewAllowlistCache() *AllowlistCache {
	return &AllowlistCache{
		repos: make(map[string]*claudetownv1alpha1.ClaudeRepository),
	}
}

// Set adds or updates a repository in the cache, keyed by "owner/repo".
func (c *AllowlistCache) Set(fullName string, repo *claudetownv1alpha1.ClaudeRepository) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repos[fullName] = repo.DeepCopy()
}

// Delete removes a repository from the cache.
func (c *AllowlistCache) Delete(fullName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.repos, fullName)
}

// IsAllowed returns true if the given "owner/repo" is in the cache.
func (c *AllowlistCache) IsAllowed(fullName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.repos[fullName]
	return ok
}

// Get returns the ClaudeRepository for the given "owner/repo", or nil if not found.
func (c *AllowlistCache) Get(fullName string) *claudetownv1alpha1.ClaudeRepository {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if repo, ok := c.repos[fullName]; ok {
		return repo.DeepCopy()
	}
	return nil
}
