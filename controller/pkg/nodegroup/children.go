package nodegroup

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

func machineOwnedByClaim(machine *unstructured.Unstructured, child *v1alpha1.ProvisionedNodeClaim) bool {
	for _, owner := range machine.GetOwnerReferences() {
		if owner.APIVersion == v1alpha1.GroupVersion.String() && owner.Kind == "ProvisionedNodeClaim" && owner.Name == child.Name && owner.UID == child.UID {
			return true
		}
	}
	return false
}

const (
	GroupUIDLabel     = "cloud-provisioning.appmana.com/node-group-uid"
	OrdinalAnnotation = "cloud-provisioning.appmana.com/node-group-ordinal"
)

func childName(group *v1alpha1.ProvisionedNodeGroupClaim, ordinal int) (string, error) {
	if group == nil || group.UID == "" || len(validation.IsValidLabelValue(string(group.UID))) != 0 || len(validation.IsDNS1123Subdomain(group.Name)) != 0 || len(validation.IsDNS1123Label(group.Namespace)) != 0 || ordinal < 0 || ordinal > 2147483647 {
		return "", fmt.Errorf("group identity and a nonnegative int32 ordinal required")
	}
	prefix := strings.ReplaceAll(group.Name, ".", "-")
	if len(prefix) > 30 {
		prefix = prefix[:30]
	}
	prefix = strings.TrimRight(prefix, "-")
	hash := sha256.Sum256([]byte(group.UID))
	return fmt.Sprintf("%s-%x-%d", prefix, hash[:8], ordinal), nil
}

// BuildChild copies provider-independent intent. A recreated group gets a new
// name prefix through its UID, even if its display name and template are reused.
// The caller must commit creation intent before creating this object.
func BuildChild(group *v1alpha1.ProvisionedNodeGroupClaim, ordinal int) (*v1alpha1.ProvisionedNodeClaim, error) {
	name, e := childName(group, ordinal)
	if e != nil {
		return nil, e
	}
	if !group.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("cannot provision for a deleting group")
	}
	child := &v1alpha1.ProvisionedNodeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ProvisionedNodeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: group.Namespace, Labels: map[string]string{GroupUIDLabel: string(group.UID)}, Annotations: map[string]string{OrdinalAnnotation: strconv.Itoa(ordinal)}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(group, v1alpha1.GroupVersion.WithKind("ProvisionedNodeGroupClaim"))}},
	}
	group.Spec.Template.Spec.DeepCopyInto(&child.Spec)
	return child, nil
}

// ObserveChild validates the controller owner, namespace, stable name, and slot.
// A label alone never grants ownership. Terminating claims continue to occupy
// their slots until the API reports them absent. Existing templates are retained.
func ObserveChild(group *v1alpha1.ProvisionedNodeGroupClaim, child *v1alpha1.ProvisionedNodeClaim) (Child, error) {
	if group == nil || child == nil || child.UID == "" {
		return Child{}, fmt.Errorf("persisted group and child required")
	}
	ordinal, e := strconv.Atoi(child.Annotations[OrdinalAnnotation])
	if e != nil || strconv.Itoa(ordinal) != child.Annotations[OrdinalAnnotation] {
		return Child{}, fmt.Errorf("invalid child ordinal")
	}
	name, e := childName(group, ordinal)
	if e != nil {
		return Child{}, e
	}
	owner := metav1.GetControllerOf(child)
	if owner == nil || owner.APIVersion != v1alpha1.GroupVersion.String() || owner.Kind != "ProvisionedNodeGroupClaim" || owner.Name != group.Name || owner.UID != group.UID || child.Namespace != group.Namespace || child.Name != name || child.Labels[GroupUIDLabel] != string(group.UID) {
		return Child{}, fmt.Errorf("child does not belong to this group")
	}
	return Child{Name: child.Name, OwnerUID: string(group.UID), Ordinal: ordinal, Terminating: !child.DeletionTimestamp.IsZero()}, nil
}
