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
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/executionretention"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

// historyPruneInterval spaces retention passes on one running supervisor.
const historyPruneInterval = time.Hour

// HistoryPruner applies a retention policy to one supervisor's history.
type HistoryPruner interface {
	PruneHistory(context.Context, processruntime.Target, processruntime.HistoryPolicy) error
}

// historyRetention sends each running target's executionRetention to its
// supervisor at most once per interval. The supervisor owns its history; a
// stopped target has nothing to prune. Pass times are kept per runtime Pod, so
// a replaced runtime is pruned at once, and live in memory, so a controller
// restart prunes each running supervisor once more.
type historyRetention struct {
	reader client.Reader
	pruner HistoryPruner
	mutex  sync.Mutex
	pruned map[types.UID]time.Time
	now    func() time.Time
}

func newHistoryRetention(reader client.Reader, pruner HistoryPruner, now func() time.Time) *historyRetention {
	if now == nil {
		now = time.Now
	}
	return &historyRetention{reader: reader, pruner: pruner, pruned: make(map[types.UID]time.Time), now: now}
}

// reconcile returns when the target needs its next pass. Zero means the target
// has no running runtime and needs no timed pass.
func (retention *historyRetention) reconcile(ctx context.Context, target client.Object, podName string, policy *workspacesv1alpha1.ExecutionRetentionPolicy) time.Duration {
	if retention.pruner == nil || podName == "" || !target.GetDeletionTimestamp().IsZero() {
		return 0
	}
	pod := new(corev1.Pod)
	if err := retention.reader.Get(ctx, client.ObjectKey{Namespace: target.GetNamespace(), Name: podName}, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Error(err, "Could not read runtime Pod for history retention", "pod", podName)
		}
		return 0
	}
	if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
		return 0
	}
	now := retention.now()
	retention.mutex.Lock()
	last, seen := retention.pruned[pod.UID]
	retention.mutex.Unlock()
	if seen && now.Sub(last) < historyPruneInterval {
		return historyPruneInterval - now.Sub(last)
	}
	platform, err := rcplatform.FromPod(pod)
	if err != nil {
		logf.FromContext(ctx).Error(err, "Could not resolve runtime platform for history retention", "pod", podName)
		return 0
	}
	// A failed pass is retried on the next interval rather than in a tight
	// loop: history is diagnostic.
	if err := retention.pruner.PruneHistory(ctx, platform.ProcessTarget(target.GetNamespace(), pod.Name, runtimeContainerName), executionretention.Policy(policy)); err != nil {
		logf.FromContext(ctx).Error(err, "Could not prune process history", "pod", podName)
	}
	retention.mutex.Lock()
	// Entries of replaced runtimes age out instead of accumulating.
	for uid, at := range retention.pruned {
		if now.Sub(at) > 2*historyPruneInterval {
			delete(retention.pruned, uid)
		}
	}
	retention.pruned[pod.UID] = now
	retention.mutex.Unlock()

	return historyPruneInterval
}
