package k0s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultRouteAgent is the Linux default-route Konnectivity agent k0s
// reconciles with the native-linux-egress patch (providers/k0s).
const DefaultRouteAgent = "konnectivity-agent-default-route-linux"

// WindowsEgressReady reports whether a Windows worker can join without
// breaking the API server's egress. With Konnectivity, k0s's servers pick an
// agent by destination host, then a default-route agent, then any agent; a
// Service address matches no node, so without a default-route agent a
// webhook call can go through a Windows agent, which cannot reach Services.
func WindowsEgressReady(ctx context.Context, c client.Reader) error {
	present := func(name string) (bool, error) {
		err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: name}, &appsv1.DaemonSet{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	konnectivity, err := present("konnectivity-agent")
	if err != nil || !konnectivity {
		return err
	}
	routed, err := present(DefaultRouteAgent)
	if err != nil {
		return err
	}
	if !routed {
		return fmt.Errorf("k0s routes API egress through Konnectivity with no Linux default-route agent, so a Windows worker's agent could carry webhook calls to Services it cannot reach; run k0s with the native-linux-egress patch (providers/k0s), which adds %s", DefaultRouteAgent)
	}
	return nil
}
