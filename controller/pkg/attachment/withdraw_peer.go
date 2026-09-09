package attachment

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PublishRemoteWithdrawal removes the expected peer from an observed mesh
// snapshot using its UID/resourceVersion preconditions. The caller must journal
// the peer and required consumers first. This write is not an acknowledgement;
// consumer verification and native teardown are separate steps.
func PublishRemoteWithdrawal(ctx context.Context, api client.Client, mesh *corev1.Secret, name, key string) (*corev1.Secret, error) {
	if api == nil || mesh == nil || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.Namespace == "" || mesh.Name == "" || !mesh.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("live persisted mesh snapshot required")
	}
	data, _, err := tunnel.WithdrawRemotePeer(mesh.Data, name, key)
	if err != nil {
		return nil, err
	}
	next := mesh.DeepCopy()
	next.Data = data
	// Update is deliberately attempted even for an already absent peer: the
	// source version must still match, including a concurrent same-name rejoin.
	if err := api.Update(ctx, next); err != nil {
		return nil, err
	}
	return next, nil
}
