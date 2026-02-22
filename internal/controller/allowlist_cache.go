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
	"regexp"
	"sync"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
)

// patternEntry holds a compiled regex pattern alongside its ClaudeRepository.
type patternEntry struct {
	re   *regexp.Regexp
	repo *claudetownv1alpha1.ClaudeRepository
}

// AllowlistCache is a thread-safe in-memory cache of allowed repositories.
// It supports both exact "owner/repo" matches and regex patterns.
// It is populated by the ClaudeRepositoryReconciler and read by the webhook handler.
type AllowlistCache struct {
	mu       sync.RWMutex
	exact    map[string]*claudetownv1alpha1.ClaudeRepository
	patterns map[string]*patternEntry // keyed by pattern string
}

// NewAllowlistCache creates a new empty AllowlistCache.
func NewAllowlistCache() *AllowlistCache {
	return &AllowlistCache{
		exact:    make(map[string]*claudetownv1alpha1.ClaudeRepository),
		patterns: make(map[string]*patternEntry),
	}
}

// Set adds or updates a repository in the cache.
// For pattern-based entries (RepositoryPattern set), the pattern is compiled
// as a regex. For exact-match entries, it is keyed by "owner/repo".
func (c *AllowlistCache) Set(fullName string, repo *claudetownv1alpha1.ClaudeRepository) {
	c.mu.Lock()
	defer c.mu.Unlock()

	copied := repo.DeepCopy()
	if copied.IsPattern() {
		re, err := regexp.Compile("^" + copied.Spec.RepositoryPattern + "$")
		if err != nil {
			// Invalid regex — store as exact match fallback using the pattern string.
			c.exact[fullName] = copied
			return
		}
		c.patterns[fullName] = &patternEntry{re: re, repo: copied}
	} else {
		c.exact[fullName] = copied
	}
}

// Delete removes a repository from the cache (both exact and pattern maps).
func (c *AllowlistCache) Delete(fullName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.exact, fullName)
	delete(c.patterns, fullName)
}

// IsAllowed returns true if the given "owner/repo" matches any entry in the
// cache, either by exact match or by regex pattern.
func (c *AllowlistCache) IsAllowed(fullName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check exact match first.
	if _, ok := c.exact[fullName]; ok {
		return true
	}

	// Check regex patterns.
	for _, entry := range c.patterns {
		if entry.re.MatchString(fullName) {
			return true
		}
	}

	return false
}

// Get returns the first matching ClaudeRepository for the given "owner/repo",
// checking exact matches first, then regex patterns. Returns nil if not found.
func (c *AllowlistCache) Get(fullName string) *claudetownv1alpha1.ClaudeRepository {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if repo, ok := c.exact[fullName]; ok {
		return repo.DeepCopy()
	}

	for _, entry := range c.patterns {
		if entry.re.MatchString(fullName) {
			return entry.repo.DeepCopy()
		}
	}

	return nil
}
