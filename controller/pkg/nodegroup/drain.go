package nodegroup

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func DrainNode(ctx context.Context, api client.Client, name string, uid types.UID) (bool, error) {
	return attachment.DrainNode(ctx, api, name, uid)
}
func drainPodExempt(pod *corev1.Pod) bool { return attachment.DrainPodExempt(pod) }
