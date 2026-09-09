/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClientSchemeDecodesAndOperatesOnLease(t *testing.T) {
	t.Parallel()
	scheme, err := newScheme()
	require.NoError(t, err, "build the rcctl Kubernetes scheme")
	holder := "workspace-uid"
	want := &coordinationv1.Lease{
		TypeMeta:   metav1.TypeMeta{APIVersion: coordinationv1.SchemeGroupVersion.String(), Kind: "Lease"},
		ObjectMeta: metav1.ObjectMeta{Name: "worktree-write", Namespace: "default"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(coordinationv1.SchemeGroupVersion)
	data, err := runtime.Encode(codec, want)
	require.NoError(t, err, "encode a coordination Lease")

	decoded, gvk, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	require.NoError(t, err, "decode a coordination Lease")
	require.NotNil(t, gvk, "decode the Lease GVK")
	assert.Equal(t, coordinationv1.SchemeGroupVersion.WithKind("Lease"), *gvk, "decode the Lease GVK")
	lease, ok := decoded.(*coordinationv1.Lease)
	require.True(t, ok, "decode a typed Lease")

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	require.NoError(t, kubeClient.Create(context.Background(), lease), "create a Lease through the rcctl client scheme")
	current := new(coordinationv1.Lease)
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(lease), current), "get a Lease through the rcctl client scheme")
	require.NotNil(t, current.Spec.HolderIdentity, "preserve the Lease holder")
	assert.Equal(t, holder, *current.Spec.HolderIdentity, "round-trip the Lease holder")
}
