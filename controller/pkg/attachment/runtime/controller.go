// Package runtime connects durable attachment capabilities to Kubernetes requests.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const RequestLabel = "cloud-provisioning.appmana.com/gateway-request"
const requestMeshOwner = "cloud-provisioning.appmana.com/gateway-mesh"
const requestFinalizer = "cloud-provisioning.appmana.com/gateway-request"

type Stepper interface {
	Step(context.Context, string, *attachment.GatewayRequest) (attachment.Phase, error)
}
type RequestController struct {
	Client              client.Client
	Namespace, MeshName string
	Lifecycle           Stepper
}

// Reconcile retains the request before preparing infrastructure. Its Kubernetes
// UID is part of the lease identity, so name reuse cannot adopt old resources.
// Removal of the selector label requests withdrawal just like object deletion.
func (r *RequestController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owned := slices.Contains(cm.Finalizers, requestFinalizer)
	selected := cm.Labels[RequestLabel] == r.MeshName
	if owned && cm.Annotations[requestMeshOwner] != r.MeshName {
		return ctrl.Result{}, nil
	}
	if !selected && !owned {
		return ctrl.Result{}, nil
	}
	if r.Lifecycle == nil || cm.UID == "" {
		return ctrl.Result{}, fmt.Errorf("attachment lifecycle and request UID required")
	}
	var desired *attachment.GatewayRequest
	if selected && cm.DeletionTimestamp == nil {
		desired = &attachment.GatewayRequest{}
		if err := json.Unmarshal([]byte(cm.Data["request.json"]), desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("invalid gateway request: %w", err)
		}
		if _, err := attachment.PlanGateway(*desired); err != nil {
			return ctrl.Result{}, err
		}
		if !owned {
			cm.Finalizers = append(cm.Finalizers, requestFinalizer)
			if cm.Annotations == nil {
				cm.Annotations = map[string]string{}
			}
			cm.Annotations[requestMeshOwner] = r.MeshName
			if err := r.Client.Update(ctx, cm); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
	}
	phase, err := r.Lifecycle.Step(ctx, cm.Namespace+"/"+cm.Name+"/"+string(cm.UID), desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	if desired == nil && phase == attachment.Complete {
		cm.Finalizers = slices.DeleteFunc(cm.Finalizers, func(v string) bool { return v == requestFinalizer })
		return ctrl.Result{}, r.Client.Update(ctx, cm)
	}
	// Periodic observations also detect AWS/guest drift without an API event.
	delay := 2 * time.Second
	if phase == attachment.Ready {
		delay = 30 * time.Second
	}
	return ctrl.Result{RequeueAfter: delay}, nil
}
