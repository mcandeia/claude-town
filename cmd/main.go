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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
	"github.com/marcoscandeia/claude-town/internal/controller"
	ghclient "github.com/marcoscandeia/claude-town/internal/github"
	"github.com/marcoscandeia/claude-town/internal/sandbox"
	webhookhandler "github.com/marcoscandeia/claude-town/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(claudetownv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// taskCreator implements webhookhandler.TaskCreator by creating ClaudeTask CRs
// in the cluster and delegating allowlist checks to the shared AllowlistCache.
type taskCreator struct {
	client    ctrlclient.Client
	namespace string
	allowlist *controller.AllowlistCache
	ghClient  ghclient.GitHubClient
}

func (tc *taskCreator) CreateTask(ctx context.Context, req webhookhandler.TaskRequest) error {
	task := &claudetownv1alpha1.ClaudeTask{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-", req.Owner, req.Repo),
			Namespace:    tc.namespace,
		},
		Spec: claudetownv1alpha1.ClaudeTaskSpec{
			Repository:      fmt.Sprintf("%s/%s", req.Owner, req.Repo),
			Issue:           req.Issue,
			PullRequest:     req.PullRequest,
			TaskType:        claudetownv1alpha1.ClaudeTaskType(req.TaskType),
			Branch:          req.Branch,
			Prompt:          req.Prompt,
			ReviewCommentID: req.ReviewCommentID,
		},
	}

	return tc.client.Create(ctx, task)
}

func (tc *taskCreator) IsRepoAllowed(_ context.Context, owner, repo string) (bool, error) {
	return tc.allowlist.IsAllowed(fmt.Sprintf("%s/%s", owner, repo)), nil
}

func (tc *taskCreator) IsUserAllowed(ctx context.Context, owner, repo, username string) (bool, error) {
	fullName := fmt.Sprintf("%s/%s", owner, repo)
	users, roles := tc.allowlist.GetMergedAllowlist(fullName)

	// If no restrictions configured, allow everyone.
	if len(users) == 0 && len(roles) == 0 {
		return true, nil
	}

	// Check username allowlist.
	for _, u := range users {
		if u == username {
			return true, nil
		}
	}

	// Check role allowlist.
	if len(roles) > 0 && tc.ghClient != nil {
		// Resolve per-repo client or use global.
		ghc := tc.ghClient
		if c := tc.allowlist.GetClient(fullName); c != nil {
			ghc = c
		}

		perm, err := ghc.GetUserRepoPermission(ctx, owner, repo, username)
		if err != nil {
			return false, fmt.Errorf("checking user permission: %w", err)
		}
		for _, r := range roles {
			if r == perm {
				return true, nil
			}
		}
	}

	return false, nil
}

func (tc *taskCreator) GetAutoLabels(_ context.Context, owner, repo string) []string {
	fullName := fmt.Sprintf("%s/%s", owner, repo)
	r := tc.allowlist.Get(fullName)
	if r == nil {
		return nil
	}
	return r.Spec.Labels
}

func (tc *taskCreator) ResumeClarification(ctx context.Context, req webhookhandler.TaskRequest) (bool, error) {
	var taskList claudetownv1alpha1.ClaudeTaskList
	if err := tc.client.List(ctx, &taskList, ctrlclient.InNamespace(tc.namespace)); err != nil {
		return false, fmt.Errorf("listing ClaudeTasks: %w", err)
	}

	fullRepo := fmt.Sprintf("%s/%s", req.Owner, req.Repo)
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if task.Status.Phase != claudetownv1alpha1.ClaudeTaskPhaseWaitingForClarification {
			continue
		}
		if task.Spec.Repository != fullRepo {
			continue
		}
		// Match by issue or PR number.
		if req.Issue > 0 && task.Spec.Issue != req.Issue {
			continue
		}
		if req.PullRequest > 0 && task.Spec.PullRequest != req.PullRequest {
			continue
		}

		// Fill in the answer on the last clarification exchange.
		if len(task.Status.Clarifications) > 0 {
			task.Status.Clarifications[len(task.Status.Clarifications)-1].Answer = req.ClarificationAnswer
		}
		task.Status.Phase = claudetownv1alpha1.ClaudeTaskPhaseRunning
		if err := tc.client.Status().Update(ctx, task); err != nil {
			return false, fmt.Errorf("updating task status: %w", err)
		}
		return true, nil
	}

	return false, nil
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// --- Read configuration from environment variables ---
	ghAppIDStr := os.Getenv("GITHUB_APP_ID")
	ghInstallIDStr := os.Getenv("GITHUB_INSTALLATION_ID")
	ghPrivateKey := os.Getenv("GITHUB_PRIVATE_KEY")
	ghWebhookSecret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	ghBotName := envOrDefault("GITHUB_BOT_NAME", "claude-town")
	ghPAT := os.Getenv("GITHUB_PAT")
	ghAllowedUsers := splitComma(os.Getenv("GITHUB_ALLOWED_USERS"))
	ghAllowedRoles := splitComma(os.Getenv("GITHUB_ALLOWED_ROLES"))
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	selfDNS := os.Getenv("SELF_DNS")
	sandboxNamespace := envOrDefault("SANDBOX_NAMESPACE", "claude-town-system")
	sandboxTemplateName := envOrDefault("SANDBOX_TEMPLATE_NAME", "claude-sandbox")
	webhookPort := envOrDefault("WEBHOOK_PORT", "8082")

	ghAppID, _ := strconv.ParseInt(ghAppIDStr, 10, 64)
	ghInstallID, _ := strconv.ParseInt(ghInstallIDStr, 10, 64)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "103dbcae.claude-town.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// --- Create GitHub client ---
	var githubClient ghclient.GitHubClient
	if ghAppID > 0 && ghInstallID > 0 && ghPrivateKey != "" {
		appClient, err := ghclient.NewClient(ghclient.Config{
			AppID:          ghAppID,
			InstallationID: ghInstallID,
			PrivateKey:     []byte(ghPrivateKey),
			WebhookSecret:  ghWebhookSecret,
			SelfDNS:        selfDNS,
			BotName:        ghBotName,
		})
		if err != nil {
			setupLog.Error(err, "unable to create GitHub App client")
			os.Exit(1)
		}
		githubClient = appClient

		// Register App-level webhook on startup.
		if selfDNS != "" {
			setupLog.Info("registering GitHub webhook", "url", fmt.Sprintf("https://%s/webhooks/github", selfDNS))
			if err := appClient.RegisterWebhook(context.Background()); err != nil {
				setupLog.Error(err, "failed to register GitHub webhook (non-fatal)")
			}
		}
	} else if ghPAT != "" {
		githubClient = ghclient.NewPATClient(ghPAT, ghBotName)
		setupLog.Info("using PAT-based GitHub authentication")
	} else {
		setupLog.Info("GitHub credentials not configured, running without GitHub integration")
	}

	// --- Create dynamic client for sandbox operations ---
	dynClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create dynamic client")
		os.Exit(1)
	}
	sandboxClient := sandbox.NewClient(dynClient, sandboxNamespace)

	// --- Create shared allowlist cache ---
	allowlist := controller.NewAllowlistCache()
	allowlist.SetGlobalAllowlist(ghAllowedUsers, ghAllowedRoles)

	// Build webhook URL for repo-level webhook auto-creation.
	var webhookURL string
	if selfDNS != "" {
		webhookURL = fmt.Sprintf("https://%s/webhooks/github", selfDNS)
	}

	// --- Register controllers ---
	if err := (&controller.ClaudeTaskReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		RestConfig:          mgr.GetConfig(),
		SandboxClient:       sandboxClient,
		GitHubClient:        githubClient,
		Allowlist:           allowlist,
		SandboxTemplateName: sandboxTemplateName,
		SandboxNamespace:    sandboxNamespace,
		AnthropicAPIKey:     anthropicKey,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClaudeTask")
		os.Exit(1)
	}
	if err := (&controller.ClaudeRepositoryReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Allowlist:      allowlist,
		GlobalGHClient: githubClient,
		WebhookURL:     webhookURL,
		WebhookSecret:  ghWebhookSecret,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClaudeRepository")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// --- Certificate watchers ---
	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	// --- Health checks ---
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// --- Start webhook HTTP server ---
	creator := &taskCreator{
		client:    mgr.GetClient(),
		namespace: sandboxNamespace,
		allowlist: allowlist,
		ghClient:  githubClient,
	}

	webhookHandler := webhookhandler.NewHandler(ghWebhookSecret, ghBotName, creator)

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", webhookHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	webhookHTTPServer := &http.Server{
		Addr:    ":" + webhookPort,
		Handler: mux,
	}

	go func() {
		setupLog.Info("starting webhook HTTP server", "port", webhookPort)
		if err := webhookHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "webhook HTTP server failed")
			os.Exit(1)
		}
	}()

	// --- Start manager ---
	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// envOrDefault returns the value of the environment variable named by key,
// or defaultValue if the variable is not set or empty.
func envOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// splitComma splits a comma-separated string into trimmed non-empty parts.
func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
