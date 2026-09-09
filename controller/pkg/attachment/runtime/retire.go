package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RetireWorkerRequest requests retirement of one previously resolved gateway
// request. Callers must persist its name and UID before invoking this operation.
// Success covers this request only; a node may have other network attachments.
// The existing RequestController performs withdrawal and native resource release.
func RetireWorkerRequest(ctx context.Context, api client.Client, namespace, mesh, name string, uid types.UID, worker attachment.Machine) (bool, error) {
	if api == nil || namespace == "" || mesh == "" || name == "" || uid == "" || worker.UID == "" || worker.NodeUID == "" || worker.ProviderID == "" {
		return false, fmt.Errorf("request scope and worker identity required")
	}
	id := namespace + "/" + name + "/" + string(uid)
	record, err := (attachment.ConfigMapStore{Client: api, Namespace: namespace}).Load(ctx, id)
	if err != nil {
		return false, err
	}
	matches := func(m attachment.Machine) bool {
		return m.UID == worker.UID && m.NodeUID == worker.NodeUID && m.ProviderID == worker.ProviderID
	}
	if record == nil || record.Lease == "" || !matches(record.Plan.Worker) {
		return false, fmt.Errorf("durable worker attachment identity missing or changed")
	}
	cm := &corev1.ConfigMap{}
	err = api.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm)
	if apierrors.IsNotFound(err) {
		return record.Phase == attachment.Complete, nil
	}
	if err != nil {
		return false, err
	}
	if cm.UID != uid || cm.Annotations[requestMeshOwner] != mesh {
		return false, fmt.Errorf("gateway request ownership changed")
	}
	var request attachment.GatewayRequest
	if err := json.Unmarshal([]byte(cm.Data["request.json"]), &request); err != nil {
		return false, fmt.Errorf("decode gateway request: %w", err)
	}
	if !matches(request.Worker) {
		return false, fmt.Errorf("gateway request now describes another worker")
	}
	if cm.DeletionTimestamp.IsZero() {
		rv := cm.ResourceVersion
		if err := api.Delete(ctx, cm, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
			return false, err
		}
	}
	// Even a Complete record can still have failed CAPI hook cleanup. Wait for
	// RequestController to release its finalizer and for the API to remove it.
	return false, nil
}
