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

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// UpstreamProvider runs a namespace's managed upstream: a SOCKS5 proxy for
// the Actors that set spec.upstream.managed. The proxy must refuse private,
// cluster, and metadata addresses by the address that a name resolves to,
// because actor runners hand it names with socks5h.
type UpstreamProvider interface {
	// Ensure runs the proxy while needed is true and stops it otherwise. It
	// creates nothing in a namespace that never needed it.
	Ensure(ctx context.Context, namespace string, needed bool) error
	// Endpoint returns how an actor runner reaches the proxy, or a message
	// to wait with while it does not serve yet.
	Endpoint(ctx context.Context, namespace string) (*workspacesv1alpha1.ActorUpstream, string, error)
}
