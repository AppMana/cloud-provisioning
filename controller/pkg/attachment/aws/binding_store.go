package aws

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

const bindingFinalizer = "cloud-provisioning.appmana.com/aws-forwarding-binding"

type ConfigMapBindingStore struct {
	Client    client.Client
	Namespace string
}

func (s ConfigMapBindingStore) key(lease string) client.ObjectKey {
	sum := sha256.Sum256([]byte(lease))
	return client.ObjectKey{Namespace: s.Namespace, Name: fmt.Sprintf("aws-binding-%x", sum[:16])}
}
func (s ConfigMapBindingStore) Load(ctx context.Context, lease string) (*BindingRecord, error) {
	if s.Client == nil || s.Namespace == "" || lease == "" {
		return nil, fmt.Errorf("binding client, namespace and lease required")
	}
	cm := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, s.key(lease), cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var record BindingRecord
	if err := json.Unmarshal([]byte(cm.Data["binding.json"]), &record); err != nil {
		return nil, err
	}
	if record.Binding.Lease != lease {
		return nil, fmt.Errorf("binding identity mismatch")
	}
	record.Version = string(cm.UID) + "/" + cm.ResourceVersion
	record.Deleting = cm.DeletionTimestamp != nil
	return &record, nil
}
func (s ConfigMapBindingStore) Save(ctx context.Context, old, next *BindingRecord) error {
	if s.Client == nil || s.Namespace == "" || next == nil || next.Binding.Lease == "" {
		return fmt.Errorf("binding client, namespace and record required")
	}
	key := s.key(next.Binding.Lease)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if old != nil {
		if !reflect.DeepEqual(old.Binding, next.Binding) || old.Released && !next.Released {
			return fmt.Errorf("binding is immutable and retirement is irreversible")
		}
		if err := s.Client.Get(ctx, key, cm); err != nil {
			return err
		}
		if string(cm.UID)+"/"+cm.ResourceVersion != old.Version {
			return fmt.Errorf("binding changed concurrently")
		}
	}
	if cm.DeletionTimestamp != nil && !next.Released {
		return fmt.Errorf("binding deletion requires provider release")
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["binding.json"] = string(raw)
	if next.Released {
		cm.Finalizers = slices.DeleteFunc(cm.Finalizers, func(f string) bool { return f == bindingFinalizer })
	} else if !slices.Contains(cm.Finalizers, bindingFinalizer) {
		cm.Finalizers = append(cm.Finalizers, bindingFinalizer)
	}
	if old == nil {
		return s.Client.Create(ctx, cm)
	}
	return s.Client.Update(ctx, cm)
}
