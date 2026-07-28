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

package hooks

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// outputLimit caps how much of a hook's stdout/stderr is retained. The output goes into an Event
// and a status message, both of which live in etcd; a hook that prints a megabyte of progress must
// not be able to bloat the object it is reporting on.
const outputLimit = 2048

// PodExecutor runs hook commands through the pods/exec subresource.
//
// This is the operator's ONLY exec path, and the only place it needs `pods/exec: create` — a grant
// worth naming plainly, because it is the ability to run arbitrary commands inside a tenant's
// containers. Two things bound it: the caller only ever supplies pods from the backed-up namespace
// (03-security-and-tenancy.md §5), and the commands come from that namespace's own workloads or
// from an admin-authored schedule, never from a field a tenant can point elsewhere.
type PodExecutor struct {
	cfg       *rest.Config
	clientset kubernetes.Interface
}

// NewPodExecutor builds an executor over the manager's REST config. It returns an error rather
// than panicking on a bad config so a misconfigured operator fails at startup with a message,
// instead of at the first backup with a nil dereference.
func NewPodExecutor(cfg *rest.Config) (*PodExecutor, error) {
	if cfg == nil {
		return nil, fmt.Errorf("hooks: a REST config is required to exec into pods")
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("hooks: build clientset for pod exec: %w", err)
	}
	return &PodExecutor{cfg: cfg, clientset: cs}, nil
}

// Exec runs command in one container and returns its captured output.
//
// It streams under ctx, which is what makes a hook timeout real rather than advisory: cancelling
// the context tears the SPDY stream down, so an overrunning hook stops holding a frozen database
// open. A non-zero exit status arrives as an error from the stream, carrying the command's own
// stderr — the thing an operator actually needs to read.
//
// Stdin is never attached and TTY is never requested: a hook is a non-interactive command, and a
// TTY would merge stderr into stdout and lose the distinction the error message depends on.
func (e *PodExecutor) Exec(ctx context.Context, pod types.NamespacedName, container string, command []string) (string, string, error) {
	req := e.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.cfg, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("prepare exec in %s/%s [%s]: %w", pod.Namespace, pod.Name, container, err)
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	outStr, errStr := truncateOutput(stdout.String()), truncateOutput(stderr.String())
	if err != nil {
		return outStr, errStr, fmt.Errorf("exec %v in %s/%s [%s]: %w%s",
			command, pod.Namespace, pod.Name, container, err, stderrSuffix(errStr))
	}
	return outStr, errStr, nil
}

// stderrSuffix appends the command's stderr to an error message when there is any. A hook failure
// whose message is only "command terminated with exit code 1" tells an operator nothing; the
// database's own complaint is the whole diagnosis.
func stderrSuffix(stderr string) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return ": " + s
	}
	return ""
}

// truncateOutput bounds captured output, marking that it was cut.
func truncateOutput(s string) string {
	if len(s) <= outputLimit {
		return s
	}
	return s[:outputLimit] + "... (truncated)"
}
