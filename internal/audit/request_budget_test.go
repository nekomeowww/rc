package audit

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPruneRequestBudget(t *testing.T) {
	// ROOT CAUSE: two candidates triggered 39 LISTs after review (13*(N+1)).
	// Review now scans once; execution only GETs each record, target and result.
	for _, count := range []int{1, 2, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			objects := make([]client.Object, count)
			for i := range objects {
				objects[i] = terminalRecord(fmt.Sprintf("record-%03d", i))
			}
			base := fixtureClient(t, objects...)
			lists, gets, deletes := 0, 0, 0
			kube := interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gets++
					return c.Get(ctx, k, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					return c.Delete(ctx, obj, opts...)
				},
			})
			review := reviewPlan(t, kube)
			require.Equal(t, 13, lists)
			require.Zero(t, gets)
			_, err := Prune(t.Context(), kube, review, auditNow)
			require.NoError(t, err)
			t.Logf("review + prune: N=%d LIST=%d GET=%d DELETE=%d", count, lists, gets, deletes)
			require.Equal(t, 13, lists, "candidate revalidation must not scan the inventory")
			require.Equal(t, 3*count, gets)
			require.Equal(t, count, deletes)
		})
	}
}
