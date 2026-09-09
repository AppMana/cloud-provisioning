package attachment

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CheckPeerPublication must run after reading the mesh Secret and before an
// optimistic-lock patch of that snapshot. Together these checks prevent an
// in-flight writer from restoring a peer after a drain marker and withdrawal.
// The reader must bypass the informer cache.
func CheckPeerPublication(ctx context.Context, reader client.Reader, observed *unstructured.Unstructured) error {
	if observed == nil || observed.GetUID() == "" {
		return fmt.Errorf("persisted Machine identity required for peer publication")
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(observed.GroupVersionKind())
	if err := reader.Get(ctx, client.ObjectKeyFromObject(observed), current); err != nil {
		return err
	}
	if current.GetUID() != observed.GetUID() {
		return fmt.Errorf("Machine identity changed before peer publication")
	}
	if !current.GetDeletionTimestamp().IsZero() || current.GetAnnotations()[DrainIntentAnnotation] != "" {
		return fmt.Errorf("Machine is retiring; peer publication refused")
	}
	return nil
}
