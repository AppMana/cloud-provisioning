package attachment

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PublicationMembership reconciles live recipients against retained identities.
// Implementations must prove retirement, not infer it from missing ready pods.
type PublicationMembership interface {
	Refresh(context.Context, Record, *corev1.Secret, []ConsumerTarget) ([]ConsumerTarget, error)
}

// Refresh preserves the immutable publication journal and rechecks retirement
// against authoritative identities on every pass. A remote recipient can retire
// only after its Node and adoption Secret UIDs are gone and its WireGuard key is
// absent from the mesh. Every currently published recipient must then acknowledge
// the new snapshot, including a same-name replacement. Attachment participants
// still have to resolve to their original CAPI identities through Resolve.
func (r MeshConsumerResolver) Refresh(ctx context.Context, record Record, mesh *corev1.Secret, retained []ConsumerTarget) ([]ConsumerTarget, error) {
	if mesh == nil || string(mesh.UID) != r.SecretUID || mesh.Namespace != r.Namespace {
		return nil, fmt.Errorf("membership mesh identity changed")
	}
	current, err := r.Resolve(ctx, record)
	if err != nil {
		return nil, err
	}
	for _, old := range retained {
		found := false
		for _, next := range current.Consumers {
			if reflect.DeepEqual(old, next) {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if err := r.retiredRemote(ctx, mesh, old); err != nil {
			return nil, err
		}
	}
	// Resolve read its own snapshot. Bind membership to the projection snapshot
	// used by the caller; Applied will recheck it after all acknowledgements.
	latest := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(mesh), latest); err != nil {
		return nil, err
	}
	if latest.UID != mesh.UID || latest.DeletionTimestamp != nil || !samePublicationSource(mesh.Data, latest.Data) {
		return nil, fmt.Errorf("mesh changed during membership observation")
	}
	return current.Consumers, nil
}

func (r MeshConsumerResolver) retiredRemote(ctx context.Context, mesh *corev1.Secret, old ConsumerTarget) error {
	if old.Site || old.NodeName == "" || old.NodeUID == "" || old.MachineName == "" || old.SecretUID == "" || old.PublicKey == "" {
		return fmt.Errorf("consumer %s has no provable remote retirement", old.NodeName)
	}
	for key, value := range mesh.Data {
		if (strings.HasPrefix(key, tunnel.PeerPublicKeyPrefix) || strings.HasPrefix(key, tunnel.NodePublicKeyPrefix)) && strings.TrimSpace(string(value)) == old.PublicKey {
			return fmt.Errorf("consumer %s key is still published", old.NodeName)
		}
	}
	node := &corev1.Node{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: old.NodeName}, node); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	} else if string(node.UID) == old.NodeUID {
		return fmt.Errorf("consumer %s Node identity still exists", old.NodeName)
	}
	secret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: tunnel.AdoptionSecretName(old.MachineName)}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	} else if string(secret.UID) == old.SecretUID {
		return fmt.Errorf("consumer %s adoption identity still exists", old.NodeName)
	}
	return nil
}

func (p GatewayPublication) recipients(ctx context.Context, record Record, mesh *corev1.Secret, intent *PublicationIntent) ([]ConsumerTarget, *PublicationIntent, error) {
	if membership, ok := p.Resolver.(PublicationMembership); ok {
		targets, err := membership.Refresh(ctx, record, mesh, intent.Consumers)
		if err != nil {
			return nil, intent, err
		}
		next := *intent
		next.Consumers = append([]ConsumerTarget(nil), intent.Consumers...)
		for _, target := range targets {
			found := false
			for _, retained := range next.Consumers {
				if reflect.DeepEqual(target, retained) {
					found = true
					break
				}
			}
			if !found {
				next.Consumers = append(next.Consumers, target)
			}
		}
		if len(next.Consumers) != len(intent.Consumers) {
			// Persist each new recipient before accepting its acknowledgement.
			// Later removal must prove retirement for this identity as well.
			if err := p.Store.Save(ctx, intent, &next); err != nil {
				return nil, intent, err
			}
			intent, err = p.Store.Load(ctx, record.LeaseID())
			if err != nil {
				return nil, intent, err
			}
			if intent == nil || intent.Retired || intent.Deleting {
				return nil, intent, fmt.Errorf("recipient history is absent or retiring")
			}
		}
		return targets, intent, nil
	}
	return intent.Consumers, intent, nil
}
