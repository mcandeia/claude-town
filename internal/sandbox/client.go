package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// GVR for SandboxClaim custom resource.
var SandboxClaimGVR = schema.GroupVersionResource{
	Group:    "extensions.agents.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "sandboxclaims",
}

// GVR for Sandbox custom resource.
var SandboxGVR = schema.GroupVersionResource{
	Group:    "agents.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "sandboxes",
}

// PodNameAnnotation is the annotation key used to store the pod name on a Sandbox resource.
const PodNameAnnotation = "agents.x-k8s.io/pod-name"

// sanitizeRe matches characters that are not lowercase alphanumeric or dashes.
var sanitizeRe = regexp.MustCompile(`[^a-z0-9-]+`)

// ReadyResult contains the information returned when a SandboxClaim becomes ready.
type ReadyResult struct {
	SandboxName string
	PodName     string
}

// ClaimName generates a deterministic, DNS-safe name for a SandboxClaim
// based on the repository owner, repository name, and issue number.
func ClaimName(owner, repo string, issue int) string {
	raw := fmt.Sprintf("%s-%s-issue-%d", owner, repo, issue)
	name := strings.ToLower(raw)
	name = sanitizeRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	return name
}

// BuildClaim constructs an unstructured SandboxClaim object ready for creation.
func BuildClaim(name, templateName, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": SandboxClaimGVR.Group + "/" + SandboxClaimGVR.Version,
			"kind":       "SandboxClaim",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"sandboxTemplateRef": map[string]interface{}{
					"name": templateName,
				},
			},
		},
	}
}

// Client wraps the Kubernetes dynamic client for operating on Sandbox and SandboxClaim resources.
type Client struct {
	dynClient dynamic.Interface
	namespace string
}

// NewClient creates a new sandbox Client.
func NewClient(dynClient dynamic.Interface, namespace string) *Client {
	return &Client{
		dynClient: dynClient,
		namespace: namespace,
	}
}

// CreateClaim creates a SandboxClaim in the configured namespace.
func (c *Client) CreateClaim(ctx context.Context, name, templateName string) error {
	claim := BuildClaim(name, templateName, c.namespace)
	_, err := c.dynClient.Resource(SandboxClaimGVR).Namespace(c.namespace).Create(ctx, claim, metav1.CreateOptions{})
	return err
}

// DeleteClaim deletes a SandboxClaim by name. It silently ignores "not found" errors.
func (c *Client) DeleteClaim(ctx context.Context, name string) error {
	err := c.dynClient.Resource(SandboxClaimGVR).Namespace(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// WaitForReady watches the Sandbox resources until the SandboxClaim's bound Sandbox
// has a Ready condition set to True. It returns the sandbox name and the pod name
// from the PodNameAnnotation.
func (c *Client) WaitForReady(ctx context.Context, claimName string, timeout time.Duration) (*ReadyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// First, get the claim to find the sandbox reference.
	claim, err := c.dynClient.Resource(SandboxClaimGVR).Namespace(c.namespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get SandboxClaim %q: %w", claimName, err)
	}

	sandboxName, found, err := unstructured.NestedString(claim.Object, "status", "sandbox", "Name")
	if err != nil || !found || sandboxName == "" {
		// Watch the claim until sandboxName is populated.
		sandboxName, err = c.waitForSandboxName(ctx, claimName)
		if err != nil {
			return nil, err
		}
	}

	// Check if the Sandbox is already ready before watching.
	existing, err := c.dynClient.Resource(SandboxGVR).Namespace(c.namespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if err == nil && isSandboxReady(existing) {
		podName := existing.GetAnnotations()[PodNameAnnotation]
		return &ReadyResult{
			SandboxName: sandboxName,
			PodName:     podName,
		}, nil
	}

	// Watch the Sandbox resource until it becomes ready.
	watcher, err := c.dynClient.Resource(SandboxGVR).Namespace(c.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + sandboxName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to watch Sandbox %q: %w", sandboxName, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for Sandbox %q to become ready: %w", sandboxName, ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, fmt.Errorf("watch channel closed for Sandbox %q", sandboxName)
			}
			if event.Type == watch.Error {
				return nil, fmt.Errorf("watch error for Sandbox %q", sandboxName)
			}

			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			if isSandboxReady(obj) {
				podName := obj.GetAnnotations()[PodNameAnnotation]
				return &ReadyResult{
					SandboxName: sandboxName,
					PodName:     podName,
				}, nil
			}
		}
	}
}

// waitForSandboxName watches the SandboxClaim until status.sandboxName is populated.
func (c *Client) waitForSandboxName(ctx context.Context, claimName string) (string, error) {
	// Re-check current state before watching.
	claim, getErr := c.dynClient.Resource(SandboxClaimGVR).Namespace(c.namespace).Get(ctx, claimName, metav1.GetOptions{})
	if getErr == nil {
		if name, found, err := unstructured.NestedString(claim.Object, "status", "sandbox", "Name"); err == nil && found && name != "" {
			return name, nil
		}
	}

	watcher, err := c.dynClient.Resource(SandboxClaimGVR).Namespace(c.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + claimName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to watch SandboxClaim %q: %w", claimName, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for sandboxName on SandboxClaim %q: %w", claimName, ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return "", fmt.Errorf("watch channel closed for SandboxClaim %q", claimName)
			}
			if event.Type == watch.Error {
				return "", fmt.Errorf("watch error for SandboxClaim %q", claimName)
			}

			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			sandboxName, found, err := unstructured.NestedString(obj.Object, "status", "sandbox", "Name")
			if err == nil && found && sandboxName != "" {
				return sandboxName, nil
			}
		}
	}
}

// isSandboxReady checks whether the Sandbox has a "Ready" condition set to "True".
func isSandboxReady(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}

	for _, c := range conditions {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(condMap, "type")
		condStatus, _, _ := unstructured.NestedString(condMap, "status")
		if condType == "Ready" && condStatus == "True" {
			return true
		}
	}
	return false
}

// podGVR is the GVR for core v1 Pods.
var podGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "pods",
}

// GetPodIP retrieves the IP address of the given pod from its status.
func (c *Client) GetPodIP(ctx context.Context, podName string) (string, error) {
	pod, err := c.dynClient.Resource(podGVR).Namespace(c.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get Pod %q: %w", podName, err)
	}

	podIP, found, err := unstructured.NestedString(pod.Object, "status", "podIP")
	if err != nil {
		return "", fmt.Errorf("failed to read podIP from Pod %q: %w", podName, err)
	}
	if !found || podIP == "" {
		return "", fmt.Errorf("pod %q does not have an IP address yet", podName)
	}

	return podIP, nil
}
