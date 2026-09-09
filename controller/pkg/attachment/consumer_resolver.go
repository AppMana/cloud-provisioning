package attachment

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"net/netip"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strings"
)

// MeshConsumerResolver retains every published site and remote participant.
// It does not filter recipients by pod availability or Ready status.
type MeshConsumerResolver struct {
	Reader                           client.Reader
	Namespace, SecretName, SecretUID string
}

func (r MeshConsumerResolver) Resolve(ctx context.Context, record Record) (*PublicationIntent, error) {
	if r.Reader == nil || r.Namespace == "" || r.SecretName == "" || r.SecretUID == "" || record.ID == "" || record.Lease == "" || record.Digest == "" {
		return nil, fmt.Errorf("consumer resolver scope and attachment required")
	}
	mesh := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.SecretName}, mesh); err != nil {
		return nil, err
	}
	if string(mesh.UID) != r.SecretUID || mesh.DeletionTimestamp != nil {
		return nil, fmt.Errorf("consumer mesh identity changed")
	}
	nodes := &corev1.NodeList{}
	if err := r.Reader.List(ctx, nodes); err != nil {
		return nil, err
	}
	byName := map[string]*corev1.Node{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		byName[node.Name] = node
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"})
	if err := r.Reader.List(ctx, machines, client.InNamespace(r.Namespace)); err != nil {
		return nil, err
	}
	byMachine := map[string]*unstructured.Unstructured{}
	for i := range machines.Items {
		m := &machines.Items[i]
		byMachine[m.GetName()] = m
	}
	sites := map[string]bool{}
	remotes := map[string]bool{}
	for key := range mesh.Data {
		if strings.HasPrefix(key, tunnel.NodePublicKeyPrefix) {
			sites[strings.TrimPrefix(key, tunnel.NodePublicKeyPrefix)] = true
		}
		if strings.HasPrefix(key, tunnel.SiteAddressesPrefix) {
			sites[strings.TrimPrefix(key, tunnel.SiteAddressesPrefix)] = true
		}
		if strings.HasPrefix(key, tunnel.PeerPublicKeyPrefix) {
			remotes[strings.TrimPrefix(key, tunnel.PeerPublicKeyPrefix)] = true
		}
	}
	intent := &PublicationIntent{Lease: record.LeaseID()}
	keysByUID := map[string]string{}
	for name := range sites {
		node := byName[name]
		key := strings.TrimSpace(string(mesh.Data[tunnel.NodePublicKeyPrefix+name]))
		if node == nil || node.UID == "" || node.DeletionTimestamp != nil || (key == "" && len(mesh.Data[tunnel.SiteAddressesPrefix+name]) == 0) {
			return nil, fmt.Errorf("site consumer %s lacks a current Node/key identity", name)
		}
		intent.Consumers = append(intent.Consumers, ConsumerTarget{NodeName: name, NodeUID: string(node.UID), Site: true, PublicKey: key})
	}
	for name := range remotes {
		machine := byMachine[name]
		key := strings.TrimSpace(string(mesh.Data[tunnel.PeerPublicKeyPrefix+name]))
		if machine == nil || machine.GetUID() == "" || (machine.GetDeletionTimestamp() != nil && !HasDeletionHold(machine)) || key == "" {
			return nil, fmt.Errorf("remote consumer %s lacks a current Machine/key identity", name)
		}
		provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
		nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
		node := byName[nodeName]
		if node == nil && nodeName == "" && provider != "" {
			for _, candidate := range byName {
				if candidate.Spec.ProviderID == provider {
					if node != nil {
						return nil, fmt.Errorf("ambiguous provider association for %s", name)
					}
					node = candidate
				}
			}
		}
		if node == nil || node.UID == "" || node.DeletionTimestamp != nil || provider == "" || node.Spec.ProviderID != provider {
			return nil, fmt.Errorf("remote consumer %s Node association changed", name)
		}
		for _, planned := range []Machine{record.Plan.Worker, record.Plan.Gateway} {
			if planned.UID == string(machine.GetUID()) && (planned.NodeUID != string(node.UID) || planned.ProviderID != provider) {
				return nil, fmt.Errorf("attachment participant %s replaced", name)
			}
		}
		adoption := &corev1.Secret{}
		if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: tunnel.AdoptionSecretName(name)}, adoption); err != nil {
			return nil, err
		}
		if adoption.UID == "" || adoption.DeletionTimestamp != nil {
			return nil, fmt.Errorf("remote adoption identity missing")
		}
		address := strings.SplitN(strings.TrimSpace(machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"]), "/", 2)[0]
		if address == "" {
			return nil, fmt.Errorf("remote tunnel identity missing")
		}
		intent.Consumers = append(intent.Consumers, ConsumerTarget{NodeName: node.Name, NodeUID: string(node.UID), MachineName: name, TunnelAddress: address, PublicKey: key, SecretUID: string(adoption.UID)})
		keysByUID[string(machine.GetUID())] = key
	}
	worker, gateway := keysByUID[record.Plan.Worker.UID], keysByUID[record.Plan.Gateway.UID]
	if worker == "" || gateway == "" || worker == gateway || len(intent.Consumers) == 0 {
		return nil, fmt.Errorf("attachment participants not uniquely published")
	}
	intent.Projection = tunnel.GatewayProjection{Lease: intent.Lease, WorkerKey: worker, GatewayKey: gateway, WorkerAddress: record.Plan.Worker.Address, WorkerHost: record.Plan.WorkerHost, DirectHosts: append([]netip.Prefix(nil), record.Plan.DirectHosts...)}
	sort.Slice(intent.Consumers, func(i, j int) bool { return intent.Consumers[i].NodeName < intent.Consumers[j].NodeName })
	return intent, nil
}
