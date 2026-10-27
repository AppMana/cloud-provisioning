package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RequestSource derives a gateway request for two Machines.
type RequestSource interface {
	Request(ctx context.Context, worker, gateway string) (attachment.GatewayRequest, error)
}

// RequestName is the gateway request a claim's gateway attachment is held by.
func RequestName(claim string) string { return "gateway-" + claim }

// GatewayClaims keeps the gateway request each claim's spec.gateway implies:
// derived from the claim's Machine and its gateway's, owned by the claim,
// and selected by RequestController. Deriving again on every pass carries
// changes in the site into the request; a pass that cannot derive leaves the
// standing request alone, since a Machine being replaced is held by the
// attachment's own lifetime.
type GatewayClaims struct {
	Client              client.Client
	Namespace, MeshName string
	Source              RequestSource
}

func (g *GatewayClaims) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != g.Namespace {
		return ctrl.Result{}, nil
	}
	claim := &v1alpha1.ProvisionedNodeClaim{}
	if err := g.Client.Get(ctx, req.NamespacedName, claim); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	} else if err != nil {
		claim = nil
	}
	cm := &corev1.ConfigMap{}
	err := g.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: RequestName(req.Name)}, cm)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	exists := err == nil
	if exists && (claim == nil || !metav1.IsControlledBy(cm, claim)) {
		// Written by an operator, or by a claim of the same name since
		// deleted, whose request garbage collection withdraws.
		return ctrl.Result{}, nil
	}
	if claim == nil || claim.DeletionTimestamp != nil || claim.Spec.Gateway == "" {
		if !exists {
			return ctrl.Result{}, nil
		}
		// Deleting the request withdraws the attachment; its finalizer
		// holds it until the gateway's resources are released.
		return ctrl.Result{}, client.IgnoreNotFound(g.Client.Delete(ctx, cm))
	}
	gateway := &v1alpha1.ProvisionedNodeClaim{}
	if err := g.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: claim.Spec.Gateway}, gateway); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("claim %s names gateway claim %s, which does not exist", claim.Name, claim.Spec.Gateway)
		}
		return ctrl.Result{}, err
	}
	if claim.Status.MachineName == "" || gateway.Status.MachineName == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	request, err := g.Source.Request(ctx, claim.Status.MachineName, gateway.Status.MachineName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("deriving the gateway request for claim %s: %w", claim.Name, err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !exists {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: RequestName(claim.Name), Namespace: claim.Namespace,
			Labels: map[string]string{RequestLabel: g.MeshName}}, Data: map[string]string{"request.json": string(raw)}}
		if err := ctrl.SetControllerReference(claim, cm, g.Client.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := g.Client.Create(ctx, cm); err != nil {
			return ctrl.Result{}, err
		}
	} else if cm.Data["request.json"] != string(raw) || cm.Labels[RequestLabel] != g.MeshName {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[RequestLabel] = g.MeshName
		cm.Data = map[string]string{"request.json": string(raw)}
		if err := g.Client.Update(ctx, cm); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
