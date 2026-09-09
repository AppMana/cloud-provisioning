package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

const NativeTransportOwner = "cloud-provisioning.appmana.com/native-transport"
const calicoAddress = "projectcalico.org/IPv4Address"

type NativeTransportRecord struct {
	Lease, NodeUID, ProviderID, Original, Native string
	HadOriginal                                  bool
}
type TransportObserver interface {
	Ready(context.Context, Machine, string) (bool, error)
}
type CalicoTransport struct {
	Client   client.Client
	Observer TransportObserver
}

// NativeTransportOwned validates the journal identity before another controller
// defers address ownership. A malformed marker must not silently disable repair.
func NativeTransportOwned(node *corev1.Node) (bool, error) {
	raw := node.Annotations[NativeTransportOwner]
	if raw == "" {
		return false, nil
	}
	var r NativeTransportRecord
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return false, err
	}
	if r.Lease == "" || r.NodeUID != string(node.UID) || r.ProviderID != node.Spec.ProviderID || r.Native == "" {
		return false, fmt.Errorf("native transport journal identity mismatch")
	}
	return true, nil
}
func (c CalicoTransport) node(ctx context.Context, m Machine) (*corev1.Node, error) {
	if c.Client == nil || m.NodeUID == "" || m.ProviderID == "" {
		return nil, fmt.Errorf("CNI client and worker identity required")
	}
	nodes := &corev1.NodeList{}
	if err := c.Client.List(ctx, nodes); err != nil {
		return nil, err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if string(node.UID) == m.NodeUID {
			if node.Spec.ProviderID != m.ProviderID {
				return nil, fmt.Errorf("worker provider identity changed")
			}
			return node, nil
		}
	}
	return nil, nil
}

// The annotation and its original value are committed atomically on the same
// Node. Deleting that Node deletes the target itself; replacement UIDs are never
// restored. Calico may rewrite only the prefix length of the selected address.
func (c CalicoTransport) Ensure(ctx context.Context, r Record) (bool, error) {
	if r.ID == "" || r.Lease == "" || r.Digest == "" || !r.Plan.Worker.Address.Is4() || !r.Plan.Worker.Subnet.IsValid() || !r.Plan.Worker.Subnet.Contains(r.Plan.Worker.Address) {
		return false, fmt.Errorf("complete IPv4 attachment required")
	}
	if c.Observer == nil {
		return false, fmt.Errorf("native CNI observer required")
	}
	node, err := c.node(ctx, r.Plan.Worker)
	if err != nil {
		return false, err
	}
	if node == nil || node.DeletionTimestamp != nil {
		return false, fmt.Errorf("worker absent or deleting")
	}
	var owner NativeTransportRecord
	if raw := node.Annotations[NativeTransportOwner]; raw != "" {
		if _, err = NativeTransportOwned(node); err != nil {
			return false, err
		}
		if err = json.Unmarshal([]byte(raw), &owner); err != nil {
			return false, err
		}
		if owner.Lease != r.LeaseID() || owner.Native != r.Plan.Worker.Address.String() {
			return false, fmt.Errorf("worker transport belongs to another lease")
		}
	} else {
		owner = NativeTransportRecord{Lease: r.LeaseID(), NodeUID: string(node.UID), ProviderID: node.Spec.ProviderID, Native: r.Plan.Worker.Address.String()}
		owner.Original, owner.HadOriginal = node.Annotations[calicoAddress]
		raw, err := json.Marshal(owner)
		if err != nil {
			return false, err
		}
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[NativeTransportOwner] = string(raw)
		node.Annotations[calicoAddress] = owner.Native + fmt.Sprintf("/%d", r.Plan.Worker.Subnet.Bits())
		if err = c.Client.Update(ctx, node); err != nil {
			return false, err
		}
	}
	if strings.SplitN(node.Annotations[calicoAddress], "/", 2)[0] != owner.Native {
		return false, fmt.Errorf("native CNI address changed outside its owner")
	}
	return c.Observer.Ready(ctx, r.Plan.Worker, owner.Native)
}
func (c CalicoTransport) Restore(ctx context.Context, r Record) (bool, error) {
	node, err := c.node(ctx, r.Plan.Worker)
	if err != nil {
		return false, err
	}
	if node == nil {
		return true, nil
	}
	raw := node.Annotations[NativeTransportOwner]
	if raw == "" {
		return true, nil
	}
	if _, err = NativeTransportOwned(node); err != nil {
		return false, err
	}
	var owner NativeTransportRecord
	if err = json.Unmarshal([]byte(raw), &owner); err != nil {
		return false, err
	}
	if owner.Lease != r.LeaseID() {
		return false, fmt.Errorf("cannot restore another lease's CNI address")
	}
	if strings.SplitN(node.Annotations[calicoAddress], "/", 2)[0] != owner.Native {
		return false, fmt.Errorf("refusing to overwrite externally changed CNI address")
	}
	if owner.HadOriginal {
		node.Annotations[calicoAddress] = owner.Original
	} else {
		delete(node.Annotations, calicoAddress)
	}
	delete(node.Annotations, NativeTransportOwner)
	if err = c.Client.Update(ctx, node); err != nil {
		return false, err
	}
	return true, nil
}
