package claim

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Failed observations are unknown, not absent. These fake-client cases exercise
// reconciliation across read failures and recovery, retaining claim ownership
// and downstream resources. They do not run CAPI or prove EC2 termination;
// deletion preconditions are covered separately against a real API server.
func TestDeletionObservationFailureAndRecovery(t *testing.T) {
	for _, stage := range []string{"Machine", "AWSMachine", "NodeList"} {
		for _, fault := range []struct {
			name string
			err  error
		}{
			{"timeout", apierrors.NewTimeoutError("read timed out", 1)},
			{"unavailable", apierrors.NewServiceUnavailable("API unavailable")},
			{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "test"}, "worker", errors.New("denied"))},
			{"deadline", context.DeadlineExceeded},
		} {
			t.Run(stage+"/"+fault.name, func(t *testing.T) {
				ctx := context.Background()
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				claim := fakeClaim("observation-worker")
				r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), fakeNode())
				must(reconcileClaim(t, r, claim))
				key := client.ObjectKeyFromObject(claim)
				must(r.Get(ctx, key, claim))
				must(r.Delete(ctx, claim))
				must(r.Get(ctx, key, claim))
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "retained-worker", Annotations: map[string]string{ClaimAnnotation: claim.Namespace + "/" + claim.Name}}}
				must(r.Create(ctx, node))
				machine := &unstructured.Unstructured{}
				machine.SetGroupVersionKind(machineGVK)
				must(r.Get(ctx, key, machine))
				infra := &unstructured.Unstructured{}
				infra.SetGroupVersionKind(joinaws.Provider{}.GVK())
				must(r.Get(ctx, key, infra))
				if stage != "Machine" {
					must(r.Delete(ctx, machine))
				}
				if stage == "NodeList" {
					must(r.Delete(ctx, infra))
				}
				reader := &deletionFaultReader{Reader: r.Reader, kind: stage, err: fault.err}
				for attempt := 0; attempt < 3; attempt++ {
					restarted := *r
					restarted.Reader = reader
					_, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: key})
					if !errors.Is(err, fault.err) {
						t.Fatalf("lost observation error: %v", err)
					}
					current := &v1alpha1.ProvisionedNodeClaim{}
					must(r.Get(ctx, key, current))
					if !reflect.DeepEqual(claim, current) {
						t.Fatal("failed observation changed the deleting claim")
					}
					currentNode := &corev1.Node{}
					must(r.Get(ctx, client.ObjectKeyFromObject(node), currentNode))
					if !reflect.DeepEqual(node, currentNode) {
						t.Fatal("failed observation changed the Node")
					}
					for _, before := range []*unstructured.Unstructured{machine, infra} {
						if stage == "NodeList" || (stage == "AWSMachine" && before.GetKind() == "Machine") {
							continue
						}
						after := &unstructured.Unstructured{}
						after.SetGroupVersionKind(before.GroupVersionKind())
						must(r.Get(ctx, key, after))
						if !reflect.DeepEqual(before.Object, after.Object) {
							t.Fatalf("failed observation changed %s", before.GetKind())
						}
					}
				}
				if reader.calls != 3 {
					t.Fatalf("fault not exercised on every restart: %d", reader.calls)
				}
				// Restore reads. Each observed resource requires its own reconciliation;
				// an accepted delete request alone must not release the finalizer.
				pending := map[string]int{"Machine": 2, "AWSMachine": 1, "NodeList": 0}[stage]
				for attempt := 0; attempt <= pending; attempt++ {
					restarted := *r
					result, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: key})
					must(err)
					current := &v1alpha1.ProvisionedNodeClaim{}
					err = r.Get(ctx, key, current)
					if attempt < pending {
						must(err)
						if !containsString(current.Finalizers, Finalizer) || result.RequeueAfter != 10*time.Second {
							t.Fatalf("lost pending deletion: %+v", result)
						}
					} else if !apierrors.IsNotFound(err) {
						t.Fatalf("recovered deletion did not complete: %v", err)
					}
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(node), &corev1.Node{}); !apierrors.IsNotFound(err) {
					t.Fatalf("Node cleanup incomplete: %v", err)
				}
			})
		}
	}
}

type deletionFaultReader struct {
	client.Reader
	kind  string
	err   error
	calls int
}

func (r *deletionFaultReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == r.kind {
		r.calls++
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func (r *deletionFaultReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.NodeList); ok && r.kind == "NodeList" {
		r.calls++
		return r.err
	}
	return r.Reader.List(ctx, list, opts...)
}
