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

package workspaces

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

const (
	inventoryConcurrency   = 8
	inventoryTimeout       = 10 * time.Second
	runtimeBridgeName      = "rc-kube"
	phaseSucceeded         = "Succeeded"
	phaseFailed            = "Failed"
	supervisorPhaseExited  = "Exited"
	supervisorPhaseStopped = "Stopped"
	supervisorPhaseRunning = "Running"
)

// HistoryLister reads the process history of one supervisor.
type HistoryLister interface {
	List(context.Context, processruntime.Target) ([]processruntime.HistoryEntry, error)
}

// InventoryTarget is one machine whose supervisor the inventory asked.
type InventoryTarget struct {
	Namespace string                                     `json:"namespace"`
	Kind      workspacesv1alpha1.WorkspaceExecTargetKind `json:"kind"`
	Name      string                                     `json:"name"`
}

// InventoryProcess is one process as its supervisor reports it.
type InventoryProcess struct {
	Target InventoryTarget `json:"target"`
	processruntime.HistoryEntry
	// Agent is derived from the command, as at start.
	Agent string `json:"agent,omitempty"`
}

// DisplayPhase maps the supervisor phase to the WorkspaceExec phase names.
func (process InventoryProcess) DisplayPhase() string {
	if process.Phase != supervisorPhaseExited {
		return process.Phase
	}
	if process.ExitCode != nil && *process.ExitCode == 0 {
		return phaseSucceeded
	}

	return phaseFailed
}

// InventoryWarning reports a running target the inventory could not read.
type InventoryWarning struct {
	Target InventoryTarget
	Err    error
}

func (warning InventoryWarning) Error() string {
	return fmt.Sprintf("%s %s/%s: %v", warning.Target.Kind, warning.Target.Namespace, warning.Target.Name, warning.Err)
}

// ProcessInventory lists processes by asking the supervisor of every running
// Workspace and WorkspaceEnvironment, like ps on each machine. It reads no
// WorkspaceExec. A stopped machine has no processes to list.
type ProcessInventory struct {
	Kube    client.Client
	History HistoryLister
}

// InventoryRequest selects the machines to ask.
type InventoryRequest struct {
	// Namespace limits the inventory; empty means every permitted namespace.
	Namespace string
	// Workspace selects one Workspace and skips WorkspaceEnvironments.
	Workspace string
}

type inventoryTarget struct {
	InventoryTarget
	runtime processruntime.Target
}

// List returns processes oldest start first. A running target that cannot be
// read becomes a warning instead of failing the whole inventory.
func (inventory *ProcessInventory) List(ctx context.Context, request InventoryRequest) ([]InventoryProcess, []InventoryWarning, error) {
	targets, err := inventory.targets(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	var (
		mutex     sync.Mutex
		processes []InventoryProcess
		warnings  []InventoryWarning
		pending   sync.WaitGroup
	)
	slots := make(chan struct{}, inventoryConcurrency)
	for _, target := range targets {
		pending.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			found, err := inventory.listTarget(ctx, target)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				warnings = append(warnings, InventoryWarning{Target: target.InventoryTarget, Err: err})
			}
			processes = append(processes, found...)
		})
	}
	pending.Wait()
	slices.SortStableFunc(processes, func(left, right InventoryProcess) int {
		if order := compareStarted(left.StartedAt, right.StartedAt); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Target.Namespace, right.Target.Namespace); order != 0 {
			return order
		}
		return cmp.Compare(left.ID, right.ID)
	})
	slices.SortFunc(warnings, func(left, right InventoryWarning) int {
		return cmp.Compare(left.Error(), right.Error())
	})

	return processes, warnings, nil
}

// Find returns the process whose ID equals or uniquely starts with idOrPrefix.
func (inventory *ProcessInventory) Find(ctx context.Context, namespace, idOrPrefix string) (InventoryProcess, error) {
	processes, warnings, err := inventory.List(ctx, InventoryRequest{Namespace: namespace})
	if err != nil {
		return InventoryProcess{}, err
	}
	matches := make([]InventoryProcess, 0, 1)
	for _, process := range processes {
		if process.ID == idOrPrefix {
			return process, nil
		}
		if strings.HasPrefix(process.ID, idOrPrefix) {
			matches = append(matches, process)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		notFound := fmt.Errorf("process %q was not found in namespace %q", idOrPrefix, namespace)
		if len(warnings) > 0 {
			return InventoryProcess{}, errors.Join(notFound, fmt.Errorf("%d running target(s) could not be read", len(warnings)))
		}
		return InventoryProcess{}, notFound
	default:
		return InventoryProcess{}, fmt.Errorf("process prefix %q is ambiguous", idOrPrefix)
	}
}

// RuntimeTarget returns the supervisor address of a running target.
func (inventory *ProcessInventory) RuntimeTarget(ctx context.Context, target InventoryTarget) (processruntime.Target, error) {
	targets, err := inventory.targets(ctx, InventoryRequest{Namespace: target.Namespace})
	if err != nil {
		return processruntime.Target{}, err
	}
	for _, candidate := range targets {
		if candidate.InventoryTarget == target {
			return candidate.runtime, nil
		}
	}

	return processruntime.Target{}, fmt.Errorf("%s %s/%s has no running runtime", target.Kind, target.Namespace, target.Name)
}

func (inventory *ProcessInventory) targets(ctx context.Context, request InventoryRequest) ([]inventoryTarget, error) {
	options := []client.ListOption{}
	if request.Namespace != "" {
		options = append(options, client.InNamespace(request.Namespace))
	}
	workspaces := new(workspacesv1alpha1.WorkspaceList)
	if err := inventory.Kube.List(ctx, workspaces, options...); err != nil {
		return nil, fmt.Errorf("list Workspaces: %w", err)
	}
	machines := make([]InventoryTarget, 0, len(workspaces.Items))
	pods := make([]string, 0, len(workspaces.Items))
	for _, workspace := range workspaces.Items {
		if request.Workspace != "" && workspace.Name != request.Workspace {
			continue
		}
		machines = append(machines, InventoryTarget{Namespace: workspace.Namespace, Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name})
		pods = append(pods, workspace.Status.RuntimePodName)
	}
	if request.Workspace == "" {
		environments := new(workspacesv1alpha1.WorkspaceEnvironmentList)
		if err := inventory.Kube.List(ctx, environments, options...); err != nil {
			return nil, fmt.Errorf("list WorkspaceEnvironments: %w", err)
		}
		for _, environment := range environments.Items {
			machines = append(machines, InventoryTarget{Namespace: environment.Namespace, Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment, Name: environment.Name})
			pods = append(pods, environment.Status.EditorPodName)
		}
	}
	targets := make([]inventoryTarget, 0, len(machines))
	for index, machine := range machines {
		if pods[index] == "" {
			continue
		}
		pod := new(corev1.Pod)
		if err := inventory.Kube.Get(ctx, client.ObjectKey{Namespace: machine.Namespace, Name: pods[index]}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get runtime Pod %s/%s: %w", machine.Namespace, pods[index], err)
		}
		if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		platform, err := rcplatform.FromPod(pod)
		if err != nil {
			return nil, fmt.Errorf("resolve runtime platform of Pod %s/%s: %w", machine.Namespace, pod.Name, err)
		}
		targets = append(targets, inventoryTarget{InventoryTarget: machine, runtime: platform.ProcessTarget(machine.Namespace, pod.Name, runtimeBridgeName)})
	}

	return targets, nil
}

func (inventory *ProcessInventory) listTarget(ctx context.Context, target inventoryTarget) ([]InventoryProcess, error) {
	listContext, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()
	entries, err := inventory.History.List(listContext, target.runtime)
	if err != nil {
		return nil, err
	}
	processes := make([]InventoryProcess, 0, len(entries))
	for _, entry := range entries {
		processes = append(processes, InventoryProcess{Target: target.InventoryTarget, HistoryEntry: entry, Agent: agentForCommand(entry.Command)})
	}

	return processes, nil
}

func agentForCommand(command []string) string {
	if len(command) == 0 {
		return ""
	}

	return AgentTypeForCommand(command[0])
}

// compareStarted orders nil after every time.
func compareStarted(left, right *time.Time) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return 1
	case right == nil:
		return -1
	default:
		return left.Compare(*right)
	}
}
