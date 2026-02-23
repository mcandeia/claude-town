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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
	ghclient "github.com/marcoscandeia/claude-town/internal/github"
)

// ClaudeRepositoryReconciler reconciles a ClaudeRepository object.
// It maintains an in-memory AllowlistCache that the webhook handler uses
// to determine if a repository is allowed to trigger tasks.
type ClaudeRepositoryReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Allowlist      *AllowlistCache
	GlobalGHClient ghclient.GitHubClient // Global fallback client
	WebhookURL     string                // e.g. "https://my-domain.com/webhooks/github"
	WebhookSecret  string                // Shared webhook secret for HMAC
}

// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=clauderepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=clauderepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=claude-town.claude-town.io,resources=clauderepositories/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ClaudeRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var repo claudetownv1alpha1.ClaudeRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("ClaudeRepository deleted", "name", req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	fullName := repo.FullName()
	logger.Info("reconciling ClaudeRepository", "fullName", fullName)

	// If being deleted, clean up webhook and remove from cache.
	if !repo.DeletionTimestamp.IsZero() {
		r.cleanupRepo(ctx, &repo, fullName)
		return ctrl.Result{}, nil
	}

	// Resolve per-repo GitHub client if patSecretRef is set.
	if repo.Spec.PATSecretRef != nil {
		patClient, err := r.buildPATClient(ctx, &repo)
		if err != nil {
			logger.Error(err, "failed to build PAT client from secret")
		} else {
			r.Allowlist.SetClient(fullName, patClient)
		}
	}

	// Auto-create webhook if not yet created.
	if repo.Status.WebhookID == 0 && r.WebhookURL != "" && !repo.IsPattern() {
		ghClient := r.resolveClient(fullName)
		if ghClient != nil {
			hookID, err := ghClient.CreateRepoWebhook(ctx, repo.Spec.Owner, repo.Spec.Repo, r.WebhookURL, r.WebhookSecret)
			if err != nil {
				logger.Error(err, "failed to create repo webhook")
			} else {
				repo.Status.WebhookID = hookID
				if err := r.Status().Update(ctx, &repo); err != nil {
					logger.Error(err, "failed to update webhook ID in status")
				}
				logger.Info("created repo webhook", "hookID", hookID)
			}
		}
	}

	// Add or update in cache.
	r.Allowlist.Set(fullName, &repo)
	logger.Info("added to allowlist", "fullName", fullName)

	return ctrl.Result{}, nil
}

func (r *ClaudeRepositoryReconciler) cleanupRepo(ctx context.Context, repo *claudetownv1alpha1.ClaudeRepository, fullName string) {
	logger := logf.FromContext(ctx)

	// Delete webhook if we created one.
	if repo.Status.WebhookID != 0 && !repo.IsPattern() {
		ghClient := r.resolveClient(fullName)
		if ghClient != nil {
			if err := ghClient.DeleteRepoWebhook(ctx, repo.Spec.Owner, repo.Spec.Repo, repo.Status.WebhookID); err != nil {
				logger.Error(err, "failed to delete repo webhook", "hookID", repo.Status.WebhookID)
			}
		}
	}

	r.Allowlist.Delete(fullName)
	r.Allowlist.DeleteClient(fullName)
	logger.Info("removed from allowlist", "fullName", fullName)
}

func (r *ClaudeRepositoryReconciler) resolveClient(fullName string) ghclient.GitHubClient {
	if c := r.Allowlist.GetClient(fullName); c != nil {
		return c
	}
	return r.GlobalGHClient
}

func (r *ClaudeRepositoryReconciler) buildPATClient(ctx context.Context, repo *claudetownv1alpha1.ClaudeRepository) (*ghclient.PATClient, error) {
	ref := repo.Spec.PATSecretRef
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: repo.Namespace}, &secret); err != nil {
		return nil, fmt.Errorf("reading secret %s: %w", ref.Name, err)
	}

	key := ref.Key
	if key == "" {
		key = "token"
	}
	token, ok := secret.Data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in secret %s", key, ref.Name)
	}

	botName := ""
	if r.GlobalGHClient != nil {
		botName = r.GlobalGHClient.BotName()
	}

	return ghclient.NewPATClient(string(token), botName), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaudeRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&claudetownv1alpha1.ClaudeRepository{}).
		Named("clauderepository").
		Complete(r)
}
