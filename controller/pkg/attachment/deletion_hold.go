package attachment

import (
	"crypto/sha256"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeletionHookKeys makes each attachment lease independently hold both CAPI
// deletion boundaries. A shared gateway must retain every other lease's hooks.
func DeletionHookKeys(lease string) (string, string) {
	sum := sha256.Sum256([]byte(lease))
	suffix := fmt.Sprintf("cloud-provisioning-%x", sum[:12])
	return "pre-drain.delete.hook.machine.cluster.x-k8s.io/" + suffix,
		"pre-terminate.delete.hook.machine.cluster.x-k8s.io/" + suffix
}

// HasDeletionHold permits observations while CAPI is deleting a Machine only
// when a complete, self-consistent pair of our lifetime hooks remains present.
// It does not waive Node, provider, cloud ownership or recipient ACK checks.
func HasDeletionHold(machine metav1.Object) bool {
	for key, lease := range machine.GetAnnotations() {
		if lease == "" {
			continue
		}
		drain, terminate := DeletionHookKeys(lease)
		if key == drain && machine.GetAnnotations()[terminate] == lease {
			return true
		}
	}
	return false
}

// DrainIntentAnnotation marks a Machine whose workloads have drained and whose
// network attachments must retire. The value identifies the owning drain action.
const DrainIntentAnnotation = "cloud-provisioning.appmana.com/drain-intent"
