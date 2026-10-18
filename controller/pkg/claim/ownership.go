package claim

import (
	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClaimUIDAnnotation binds a created Machine to the claim incarnation even when
// another controller changes ownerReferences. It is written at creation only;
// a matching name or claim status is not authority to adopt an existing object.
const ClaimUIDAnnotation = "cloud-provisioning.appmana.com/claim-uid"

// MachineOwnedByClaim admits a durable creation binding or a legacy exact owner
// reference. A partial/conflicting binding or claim owner is always rejected.
// This establishes association, not drain or deletion authorization.
func MachineOwnedByClaim(machine metav1.Object, claim *v1alpha1.ProvisionedNodeClaim) bool {
	if machine == nil || claim == nil || claim.UID == "" || machine.GetName() != claim.Name || machine.GetNamespace() != claim.Namespace {
		return false
	}
	annotations := machine.GetAnnotations()
	name, hasName := annotations[ClaimAnnotation]
	uid, hasUID := annotations[ClaimUIDAnnotation]
	if (hasName || hasUID) && (!hasName || !hasUID || name != claim.Namespace+"/"+claim.Name || uid != string(claim.UID)) {
		return false
	}
	legacy := false
	for _, owner := range machine.GetOwnerReferences() {
		if owner.Kind != "ProvisionedNodeClaim" {
			continue
		}
		if owner.APIVersion != v1alpha1.GroupVersion.String() || owner.Name != claim.Name || owner.UID != claim.UID {
			return false
		}
		legacy = true
	}
	return (hasName && hasUID) || legacy
}
