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

package pty

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecClient runs commands in sandbox pods via the Kubernetes exec API.
type ExecClient struct {
	clientset *kubernetes.Clientset
	config    *rest.Config
	namespace string
}

// NewExecClient creates a new ExecClient from the given rest config and namespace.
func NewExecClient(config *rest.Config, namespace string) (*ExecClient, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset: %w", err)
	}
	return &ExecClient{
		clientset: clientset,
		config:    config,
		namespace: namespace,
	}, nil
}

// ExecResult contains the output from a command execution.
type ExecResult struct {
	Stdout string
	Stderr string
}

// Exec runs a command in the given pod and container, returning stdout and stderr.
func (c *ExecClient) Exec(ctx context.Context, podName, containerName string, command []string) (*ExecResult, error) {
	req := c.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(c.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
			Stdin:     false,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.config, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}, err
}

// RunAndWaitForMarker executes a shell script in the pod and returns the
// combined output. It checks for the completion marker in stdout.
func (c *ExecClient) RunAndWaitForMarker(ctx context.Context, podName, containerName, script, marker string) (string, error) {
	result, err := c.Exec(ctx, podName, containerName, []string{"/bin/bash", "-c", script})
	if err != nil {
		output := result.Stdout + result.Stderr
		return output, fmt.Errorf("exec failed: %w", err)
	}

	output := result.Stdout
	if !strings.Contains(output, marker) {
		return output + result.Stderr, fmt.Errorf("marker %q not found in output", marker)
	}

	return output, nil
}

// ParsePRURL extracts a pull request URL from output that contains the
// ":::PR_URL:::" marker. It returns the URL found after the marker, or an
// empty string if no marker is present.
func ParsePRURL(output string) string {
	const marker = ":::PR_URL:::"
	idx := strings.Index(output, marker)
	if idx == -1 {
		return ""
	}

	rest := output[idx+len(marker):]
	rest = strings.TrimSpace(rest)

	// Take the first whitespace-delimited token as the URL.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
