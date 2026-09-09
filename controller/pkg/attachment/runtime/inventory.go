package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type WorkerRequestRef struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// WorkerRequests observes gateway requests in one mesh, including requests whose
// selector label was removed during retirement. An empty inventory establishes
// no gateway requests in this scope, not completion of all node networking.
// Persist these identities before retirement and recheck for additional requests
// before allowing machine removal. Shared gateway removal requires another plan.
func WorkerRequests(ctx context.Context, api client.Reader, namespace, mesh string, worker attachment.Machine) ([]WorkerRequestRef, error) {
	if api == nil || namespace == "" || mesh == "" || worker.UID == "" || worker.NodeUID == "" || worker.ProviderID == "" {
		return nil, fmt.Errorf("request scope and worker identity required")
	}
	cms := &corev1.ConfigMapList{}
	if err := api.List(ctx, cms, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	refs := []WorkerRequestRef{}
	for i := range cms.Items {
		cm := &cms.Items[i]
		if cm.Labels[RequestLabel] != mesh && cm.Annotations[requestMeshOwner] != mesh {
			continue
		}
		var req attachment.GatewayRequest
		if err := json.Unmarshal([]byte(cm.Data["request.json"]), &req); err != nil {
			return nil, fmt.Errorf("decode gateway request %s: %w", cm.Name, err)
		}
		if req.Gateway.UID == worker.UID {
			return nil, fmt.Errorf("machine is a shared gateway for request %s", cm.Name)
		}
		if req.Worker.UID != worker.UID {
			continue
		}
		if req.Worker.NodeUID != worker.NodeUID || req.Worker.ProviderID != worker.ProviderID {
			return nil, fmt.Errorf("worker identity changed in request %s", cm.Name)
		}
		if cm.UID == "" || cm.Annotations[requestMeshOwner] != mesh {
			return nil, fmt.Errorf("request %s awaits lifecycle ownership", cm.Name)
		}
		refs = append(refs, WorkerRequestRef{Name: cm.Name, UID: cm.UID})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}
