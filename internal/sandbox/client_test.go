package sandbox

import (
	"testing"
)

func TestSandboxClaimName(t *testing.T) {
	got := ClaimName("my-org", "my-repo", 42)
	want := "my-org-my-repo-issue-42"
	if got != want {
		t.Errorf("ClaimName(\"my-org\", \"my-repo\", 42) = %q; want %q", got, want)
	}
}

func TestSandboxClaimName_Sanitizes(t *testing.T) {
	got := ClaimName("My_Org", "My.Repo", 7)
	want := "my-org-my-repo-issue-7"
	if got != want {
		t.Errorf("ClaimName(\"My_Org\", \"My.Repo\", 7) = %q; want %q", got, want)
	}
}

func TestBuildSandboxClaim(t *testing.T) {
	claim := BuildClaim("test-claim", "my-template", "default")

	if claim.GetKind() != "SandboxClaim" {
		t.Errorf("expected Kind SandboxClaim, got %q", claim.GetKind())
	}

	if claim.GetName() != "test-claim" {
		t.Errorf("expected name test-claim, got %q", claim.GetName())
	}

	if claim.GetNamespace() != "default" {
		t.Errorf("expected namespace default, got %q", claim.GetNamespace())
	}

	expectedAPIVersion := "extensions.agents.x-k8s.io/v1alpha1"
	if claim.GetAPIVersion() != expectedAPIVersion {
		t.Errorf("expected apiVersion %q, got %q", expectedAPIVersion, claim.GetAPIVersion())
	}

	// Verify the template ref name is set correctly.
	templateName, found := extractTemplateRefName(claim.Object)
	if !found {
		t.Fatal("sandboxTemplateRef.name not found in spec")
	}
	if templateName != "my-template" {
		t.Errorf("expected template ref name %q, got %q", "my-template", templateName)
	}
}

// extractTemplateRefName retrieves spec.sandboxTemplateRef.name from an unstructured object.
func extractTemplateRefName(obj map[string]interface{}) (string, bool) {
	spec, ok := obj["spec"].(map[string]interface{})
	if !ok {
		return "", false
	}
	ref, ok := spec["sandboxTemplateRef"].(map[string]interface{})
	if !ok {
		return "", false
	}
	name, ok := ref["name"].(string)
	if !ok {
		return "", false
	}
	return name, true
}
