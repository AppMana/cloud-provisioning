package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CAPILifetime holds each worker and shared gateway independently per lease.
// The attachment record, committed before Protect, is the durable hook journal.
type CAPILifetime struct {
	Client                           client.Client
	Namespace, ClusterName, MeshName string
}

func (c CAPILifetime) machines(ctx context.Context) (map[string]*unstructured.Unstructured, error) {
	if c.Client == nil || c.Namespace == "" || c.ClusterName == "" || c.MeshName == "" {
		return nil, fmt.Errorf("CAPI lifetime scope required")
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"})
	if err := c.Client.List(ctx, list, client.InNamespace(c.Namespace)); err != nil {
		return nil, err
	}
	out := map[string]*unstructured.Unstructured{}
	for i := range list.Items {
		m := &list.Items[i]
		out[string(m.GetUID())] = m
	}
	return out, nil
}

func (c CAPILifetime) request(ctx context.Context, r attachment.Record) (*corev1.ConfigMap, error) {
	parts := strings.Split(r.ID, "/")
	if len(parts) != 3 || parts[0] != c.Namespace || parts[1] == "" || parts[2] == "" {
		return nil, fmt.Errorf("CAPI lifetime request identity required")
	}
	cm := &corev1.ConfigMap{}
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: parts[1]}, cm); err != nil {
		return nil, err
	}
	if string(cm.UID) != parts[2] || cm.Annotations[requestMeshOwner] != c.MeshName || !slices.Contains(cm.Finalizers, requestFinalizer) {
		return nil, fmt.Errorf("CAPI lifetime request ownership changed")
	}
	return cm, nil
}

func (c CAPILifetime) Protect(ctx context.Context, r attachment.Record) (bool, error) {
	machines, err := c.machines(ctx)
	if err != nil {
		return false, err
	}
	cm, err := c.request(ctx, r)
	if err != nil {
		return false, err
	}
	drain, terminate := attachment.DeletionHookKeys(r.LeaseID())
	retiring := cm.DeletionTimestamp != nil
	participants := []attachment.Machine{r.Plan.Worker, r.Plan.Gateway}
	for _, planned := range participants {
		m := machines[planned.UID]
		if m == nil || m.GetDeletionTimestamp() != nil {
			retiring = true
			continue
		}
		provider, _, _ := unstructured.NestedString(m.Object, "spec", "providerID")
		cluster, _, _ := unstructured.NestedString(m.Object, "spec", "clusterName")
		name, _, _ := unstructured.NestedString(m.Object, "status", "nodeRef", "name")
		node := &corev1.Node{}
		if err := c.Client.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
			return false, err
		}
		if provider != planned.ProviderID || cluster != c.ClusterName || string(node.UID) != planned.NodeUID || node.Spec.ProviderID != provider || node.DeletionTimestamp != nil {
			return false, fmt.Errorf("CAPI lifetime participant identity changed")
		}
	}
	if retiring {
		// Never prepare infrastructure or add hooks after deletion has started.
		// An already protected Machine stays alive until acknowledged withdrawal.
		if cm.DeletionTimestamp == nil {
			uid := cm.UID
			if err := c.Client.Delete(ctx, cm, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	for _, planned := range participants {
		m := machines[planned.UID]
		before := m.DeepCopy()
		a := m.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		changed := false
		for _, key := range []string{drain, terminate} {
			if value, exists := a[key]; exists && value != r.LeaseID() {
				return false, fmt.Errorf("CAPI hook ownership changed")
			}
			if a[key] != r.LeaseID() {
				a[key] = r.LeaseID()
				changed = true
			}
		}
		if !changed {
			continue
		}
		m.SetAnnotations(a)
		if err := c.Client.Patch(ctx, m, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	// Re-read after both CAS patches: deletion between participants must request
	// withdrawal, never enter the infrastructure preparation step.
	latest, err := c.machines(ctx)
	if err != nil {
		return false, err
	}
	for _, planned := range participants {
		m := latest[planned.UID]
		if m == nil || m.GetDeletionTimestamp() != nil {
			uid := cm.UID
			return true, c.Client.Delete(ctx, cm, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		}
		if m.GetAnnotations()[drain] != r.LeaseID() || m.GetAnnotations()[terminate] != r.LeaseID() {
			return false, fmt.Errorf("CAPI lifetime hooks changed")
		}
	}
	return false, nil
}

func (c CAPILifetime) Release(ctx context.Context, r attachment.Record) error {
	machines, err := c.machines(ctx)
	if err != nil {
		return err
	}
	drain, terminate := attachment.DeletionHookKeys(r.LeaseID())
	for _, planned := range []attachment.Machine{r.Plan.Worker, r.Plan.Gateway} {
		m := machines[planned.UID]
		if m == nil {
			continue
		} // Never touch a same-name replacement.
		before := m.DeepCopy()
		a := m.GetAnnotations()
		changed := false
		for _, key := range []string{drain, terminate} {
			if value, exists := a[key]; exists && value != r.LeaseID() {
				return fmt.Errorf("CAPI hook ownership changed")
			}
			if _, exists := a[key]; exists {
				delete(a, key)
				changed = true
			}
		}
		if !changed {
			continue
		}
		m.SetAnnotations(a)
		if err := c.Client.Patch(ctx, m, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

var _ attachment.Lifetime = CAPILifetime{}
