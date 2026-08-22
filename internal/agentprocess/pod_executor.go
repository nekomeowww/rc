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

package agentprocess

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// KubernetesPodExecutor implements PodExecutor with client-go SPDY exec.
type KubernetesPodExecutor struct {
	config    *rest.Config
	clientset kubernetes.Interface
}

func NewKubernetesPodExecutor(config *rest.Config) (*KubernetesPodExecutor, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes clientset for Pod exec: %w", err)
	}

	return &KubernetesPodExecutor{config: rest.CopyConfig(config), clientset: clientset}, nil
}

func (executor *KubernetesPodExecutor) Exec(ctx context.Context, target Target, command []string, input io.Reader, output io.Writer, errorOutput io.Writer, terminal bool) error {
	request := executor.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(target.Pod).
		Namespace(target.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: target.Container,
			Command:   command,
			Stdin:     input != nil,
			Stdout:    output != nil,
			Stderr:    errorOutput != nil,
			TTY:       terminal,
		}, scheme.ParameterCodec)
	stream, err := remotecommand.NewSPDYExecutor(executor.config, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("create Pod exec stream: %w", err)
	}
	if err := stream.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: input, Stdout: output, Stderr: errorOutput, Tty: terminal,
	}); err != nil {
		return fmt.Errorf("stream Pod exec: %w", err)
	}

	return nil
}
