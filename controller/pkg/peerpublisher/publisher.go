// Package peerpublisher delivers public peer state to a host owner and only
// acknowledges the controller after that owner confirms native application.
package peerpublisher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type Delivery interface {
	Submit(secretUID string, raw []byte) (applied bool, err error)
}

type Publisher struct {
	Secrets  typedcore.SecretInterface
	Name     string
	Delivery Delivery
}

func (p *Publisher) Reconcile(ctx context.Context) error {
	secret, err := p.Secrets.Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	raw := secret.Data[tunnel.CloudPeersKey]
	if len(raw) == 0 || secret.UID == "" {
		return fmt.Errorf("adoption Secret has no public payload or identity")
	}
	applied, err := p.Delivery.Submit(string(secret.UID), raw)
	if err != nil {
		return err
	}
	hash := tunnel.HashPeerList(raw)
	if !applied || secret.Annotations[tunnel.AppliedListAnnotation] == hash {
		return nil
	}
	// The resourceVersion precondition rejects acknowledgments if the controller
	// changed/recreated the Secret while this delivery was being checked.
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": secret.ResourceVersion, "uid": secret.UID, "annotations": map[string]string{tunnel.AppliedListAnnotation: hash}}})
	if err != nil {
		return err
	}
	_, err = p.Secrets.Patch(ctx, p.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
