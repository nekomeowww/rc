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

package cluster

import (
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// clusterScopedKinds lists the cluster-scoped kinds of the groups in the
// rcctl scheme. Every other kind there, including every rc CRD, is namespaced.
var clusterScopedKinds = map[schema.GroupKind]struct{}{
	{Group: corev1.GroupName, Kind: "ComponentStatus"}:    {},
	{Group: corev1.GroupName, Kind: "Namespace"}:          {},
	{Group: corev1.GroupName, Kind: "Node"}:               {},
	{Group: corev1.GroupName, Kind: "PersistentVolume"}:   {},
	{Group: rbacv1.GroupName, Kind: "ClusterRole"}:        {},
	{Group: rbacv1.GroupName, Kind: "ClusterRoleBinding"}: {},
}

// irregularResources names the resources whose plural the regular English
// rule in meta.UnsafeGuessKindToResource gets wrong.
var irregularResources = map[schema.GroupKind]string{
	{Group: corev1.GroupName, Kind: "Endpoints"}: "endpoints",
}

// newRESTMapper maps the kinds that rcctl registers in scheme without asking
// the API server. Discovery otherwise costs every rcctl command two sequential
// round trips before its first request. Kinds outside scheme use fallback.
func newRESTMapper(scheme *runtime.Scheme, fallback meta.RESTMapper) meta.RESTMapper {
	static := meta.NewDefaultRESTMapper(scheme.PrioritizedVersionsAllGroups())
	objectType := reflect.TypeFor[metav1.Object]()
	for gvk, goType := range scheme.AllKnownTypes() {
		// Lists, options, and events of the watch protocol are not resources.
		if strings.HasSuffix(gvk.Kind, "List") || !reflect.PointerTo(goType).Implements(objectType) {
			continue
		}
		plural, singular := meta.UnsafeGuessKindToResource(gvk)
		if resource, ok := irregularResources[gvk.GroupKind()]; ok {
			plural, singular = gvk.GroupVersion().WithResource(resource), gvk.GroupVersion().WithResource(resource)
		}
		scope := meta.RESTScopeNamespace
		if _, ok := clusterScopedKinds[gvk.GroupKind()]; ok {
			scope = meta.RESTScopeRoot
		}
		static.AddSpecific(gvk, plural, singular, scope)
	}

	return meta.FirstHitRESTMapper{MultiRESTMapper: meta.MultiRESTMapper{static, fallback}}
}
