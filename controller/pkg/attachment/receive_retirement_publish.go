package attachment

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PublishReceiveWithdrawal removes only the root bound to persisted retirement
// intent, after admitting every reserved journal and rechecking drain authority.
// authorize must read live lifecycle state and match the supplied Machine UID
// and drain intent. It must not infer authority merely from Machine absence.
// This is publication only: no native completion, compute deletion or release.
// Cross-object reads are not transactional; agents independently re-admit their
// full source state before cleanup. A failed/ambiguous update requires retry.
func PublishReceiveWithdrawal(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, authorize func(context.Context, RemoteWithdrawalIntent) error) (*corev1.Secret, error) {
	if authorize == nil || inventoryUID == "" {
		return nil, fmt.Errorf("withdrawal publication requires drain authority and inventory identity")
	}
	source, err := ReadPreparedReceiveRetirementSet(ctx, api, store, namespace, meshName, meshUID, machineUID)
	alreadyPublished := false
	if err != nil {
		// A lost publication response must resume from the same persisted recipients,
		// not create a new intent or treat a partial/malformed removal as success.
		source, err = ReadReceiveRetirementSet(ctx, api, store, namespace, meshName, meshUID, machineUID)
		if err != nil {
			return nil, err
		}
		alreadyPublished = true
	}
	if string(source.Metadata.UID) != inventoryUID {
		return nil, fmt.Errorf("withdrawal inventory incarnation changed")
	}
	if err := authorize(ctx, source.Withdrawal); err != nil {
		return nil, err
	}
	if err := source.Recheck(ctx, api, store); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if alreadyPublished {
		return source.Mesh.DeepCopy(), nil
	}
	next := source.Mesh.DeepCopy()
	next.Data, _, err = tunnel.WithdrawRemotePeer(next.Data, source.Withdrawal.MachineName, source.Withdrawal.PublicKey)
	if err != nil {
		return nil, err
	}
	published, err := api.CoreV1().Secrets(namespace).Update(ctx, next, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	if published.UID != source.Mesh.UID || published.DeletionTimestamp != nil || !reflect.DeepEqual(published.Data, next.Data) {
		return nil, fmt.Errorf("published withdrawal differs from admitted projection")
	}
	verified, err := ReadReceiveRetirementSet(ctx, api, store, namespace, meshName, meshUID, machineUID)
	if err != nil {
		return nil, err
	}
	if verified.Metadata.UID != source.Metadata.UID || verified.Metadata.ResourceVersion != source.Metadata.ResourceVersion || verified.MeshMetadata.ResourceVersion != published.ResourceVersion {
		return nil, fmt.Errorf("withdrawal source changed during publication")
	}
	return verified.Mesh.DeepCopy(), nil
}
