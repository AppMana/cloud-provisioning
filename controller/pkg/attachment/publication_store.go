package attachment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
)

const publicationFinalizer = "cloud-provisioning.appmana.com/gateway-publication"

type ConfigMapPublicationStore struct {
	Client    client.Client
	Namespace string
}

func (s ConfigMapPublicationStore) key(lease string) client.ObjectKey {
	sum := sha256.Sum256([]byte(lease))
	return client.ObjectKey{Namespace: s.Namespace, Name: fmt.Sprintf("gateway-publication-%x", sum[:16])}
}
func (s ConfigMapPublicationStore) Load(ctx context.Context, lease string) (*PublicationIntent, error) {
	if s.Client == nil || s.Namespace == "" || lease == "" {
		return nil, fmt.Errorf("publication client, namespace and lease required")
	}
	cm := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, s.key(lease), cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var record PublicationIntent
	if err := json.Unmarshal([]byte(cm.Data["publication.json"]), &record); err != nil {
		return nil, err
	}
	if record.Lease != lease {
		return nil, fmt.Errorf("publication identity mismatch")
	}
	record.Version = string(cm.UID) + "/" + cm.ResourceVersion
	record.Deleting = cm.DeletionTimestamp != nil
	return &record, nil
}
func (s ConfigMapPublicationStore) Save(ctx context.Context, old, next *PublicationIntent) error {
	if s.Client == nil || s.Namespace == "" || next == nil || next.Lease == "" {
		return fmt.Errorf("publication client, namespace and record required")
	}
	key := s.key(next.Lease)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if old != nil {
		if !reflect.DeepEqual(publicationIdentity(old), publicationIdentity(next)) || old.Retired && !next.Retired ||
			len(next.Consumers) < len(old.Consumers) || !reflect.DeepEqual(next.Consumers[:len(old.Consumers)], old.Consumers) ||
			old.Retired && len(next.Consumers) != len(old.Consumers) {
			return fmt.Errorf("publication is immutable and retirement is irreversible")
		}
		if err := s.Client.Get(ctx, key, cm); err != nil {
			return err
		}
		if string(cm.UID)+"/"+cm.ResourceVersion != old.Version {
			return fmt.Errorf("publication changed concurrently")
		}
	}
	if cm.DeletionTimestamp != nil && !next.Retired {
		return fmt.Errorf("publication deletion requires acknowledged withdrawal")
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["publication.json"] = string(raw)
	if next.Retired {
		cm.Finalizers = slices.DeleteFunc(cm.Finalizers, func(f string) bool { return f == publicationFinalizer })
	} else if !slices.Contains(cm.Finalizers, publicationFinalizer) {
		cm.Finalizers = append(cm.Finalizers, publicationFinalizer)
	}
	if old == nil {
		return s.Client.Create(ctx, cm)
	}
	return s.Client.Update(ctx, cm)
}

func publicationIdentity(p *PublicationIntent) PublicationIntent {
	copy := *p
	copy.Retired = false
	copy.Version = ""
	copy.Deleting = false
	// Recipient history can only grow; Save separately validates its prefix.
	copy.Consumers = nil
	return copy
}
