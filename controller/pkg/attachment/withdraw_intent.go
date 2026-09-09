package attachment

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RemoteWithdrawalIntent contains public identities only. Persist it before
// publishing removal; retries must retain its recipients even when unavailable.
type RemoteWithdrawalIntent struct {
	MeshNamespace string           `json:"meshNamespace"`
	MeshName      string           `json:"meshName"`
	MeshUID       string           `json:"meshUID"`
	SourceVersion string           `json:"sourceVersion"`
	MachineName   string           `json:"machineName"`
	MachineUID    string           `json:"machineUID"`
	PublicKey     string           `json:"publicKey"`
	DrainIntent   string           `json:"drainIntent"`
	Consumers     []ConsumerTarget `json:"consumers"`
}

// PrepareRemoteWithdrawal captures every published survivor and excludes only
// the exact retiring worker. The caller owns persistence and publication.
func (r MeshConsumerResolver) PrepareRemoteWithdrawal(ctx context.Context, name string, worker Machine, drainIntent string) (*RemoteWithdrawalIntent, error) {
	if r.Reader == nil || r.Namespace == "" || r.SecretName == "" || r.SecretUID == "" || name == "" || worker.UID == "" || worker.NodeUID == "" || worker.ProviderID == "" || drainIntent == "" {
		return nil, fmt.Errorf("mesh, bound worker and drain intent required")
	}
	mesh := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.SecretName}, mesh); err != nil {
		return nil, err
	}
	if string(mesh.UID) != r.SecretUID || !mesh.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("mesh identity changed")
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, machine); err != nil {
		return nil, err
	}
	if string(machine.GetUID()) != worker.UID || machine.GetAnnotations()[DrainIntentAnnotation] != drainIntent || !machine.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("retiring Machine identity or drain intent changed")
	}
	consumers, keys, err := r.resolveTargets(ctx, mesh, []Machine{worker})
	if err != nil {
		return nil, err
	}
	key := keys[worker.UID]
	if key == "" || string(mesh.Data[tunnel.PeerPublicKeyPrefix+name]) != key {
		return nil, fmt.Errorf("retiring peer identity missing")
	}
	intent := &RemoteWithdrawalIntent{MeshNamespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), SourceVersion: mesh.ResourceVersion, MachineName: name, MachineUID: worker.UID, PublicKey: key, DrainIntent: drainIntent}
	removed := 0
	for _, c := range consumers {
		if !c.Site && c.MachineName == name && c.NodeUID == worker.NodeUID && c.PublicKey == key {
			removed++
			continue
		}
		if c.NodeUID == worker.NodeUID || c.PublicKey == key {
			return nil, fmt.Errorf("retiring identity overlaps a survivor")
		}
		intent.Consumers = append(intent.Consumers, c)
	}
	if removed != 1 || len(intent.Consumers) == 0 {
		return nil, fmt.Errorf("exact retiring peer and surviving consumers required")
	}
	last := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(mesh), last); err != nil {
		return nil, err
	}
	if last.UID != mesh.UID || last.ResourceVersion != mesh.ResourceVersion || !last.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("mesh changed while capturing withdrawal recipients")
	}
	return intent, nil
}
