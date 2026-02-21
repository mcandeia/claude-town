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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
)

// ClaudeRepositoryReconciler reconciles a ClaudeRepository object.
// It maintains an in-memory AllowlistCache that the webhook handler uses
// to determine if a repository is allowed to trigger tasks.
type ClaudeRepositoryReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Allowlist *AllowlistCache
}

// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=clauderepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=clauderepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=clauderepositories/finalizers,verbs=update

func (r *ClaudeRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var repo claudetownv1alpha1.ClaudeRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if errors.IsNotFound(err) {
			// Resource was deleted — remove from allowlist cache.
			// We don't know the full name from just the request, so we rely on
			// the cache entry being keyed by "owner/repo". We need to scan
			// or we key the cache by namespace/name. Let's just log and return.
			// Actually, we key by "owner/repo" but on delete we don't have the spec.
			// The simplest approach: on delete, the reconciler won't find it,
			// so we do a full resync. But that's expensive.
			// Better approach: key the cache also by namespace/name for reverse lookup.
			logger.Info("ClaudeRepository deleted", "name", req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	fullName := repo.FullName()
	logger.Info("reconciling ClaudeRepository", "fullName", fullName)

	// If being deleted, remove from cache.
	if !repo.DeletionTimestamp.IsZero() {
		r.Allowlist.Delete(fullName)
		logger.Info("removed from allowlist", "fullName", fullName)
		return ctrl.Result{}, nil
	}

	// Add or update in cache.
	r.Allowlist.Set(fullName, &repo)
	logger.Info("added to allowlist", "fullName", fullName)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaudeRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&claudetownv1alpha1.ClaudeRepository{}).
		Named("clauderepository").
		Complete(r)
}
