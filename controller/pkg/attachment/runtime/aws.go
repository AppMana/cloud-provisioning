package runtime

import (
	"context"
	"fmt"
	"strconv"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	provider "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	sdk "github.com/aws/aws-sdk-go-v2/aws"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AWSConfig contains placement scope, never credentials. One elected mesh
// controller must exclusively own this AWS resource scope.
type AWSConfig struct {
	Scope           provider.Scope `json:"scope"`
	ClusterName     string         `json:"clusterName"`
	GroupID         string         `json:"groupID"`
	MeshUID         string         `json:"meshUID"`
	NativeInterface string         `json:"nativeInterface"`
	TunnelInterface string         `json:"tunnelInterface"`
}

type gatewayGuest struct {
	reader         *provider.SSMGuestReader
	native, tunnel string
}

func (g gatewayGuest) guest(r attachment.Record) provider.LinuxGatewayGuest {
	return provider.LinuxGatewayGuest{Exec: provider.SSMGatewayExec{Guest: g.reader, Machine: r.Plan.Gateway}, NativeInterface: g.native, TunnelInterface: g.tunnel}
}
func (g gatewayGuest) Ensure(ctx context.Context, r attachment.Record, b provider.ForwardingBinding) (bool, error) {
	return g.guest(r).Ensure(ctx, r, b)
}
func (g gatewayGuest) Release(ctx context.Context, r attachment.Record, b provider.ForwardingBinding) (bool, error) {
	return g.guest(r).Release(ctx, r, b)
}

func RegisterAWS(mgr ctrl.Manager, cfg AWSConfig, credentials sdk.Config, namespace, meshName, apiVIP, apiPort string) error {
	port, err := strconv.ParseUint(apiPort, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("explicit TCP API port required")
	}
	s := cfg.Scope
	if namespace == "" || meshName == "" || cfg.MeshUID == "" || cfg.ClusterName == "" || cfg.GroupID == "" || cfg.NativeInterface == "" || cfg.TunnelInterface == "" || cfg.NativeInterface == cfg.TunnelInterface || s.Account == "" || s.Region == "" || s.VPCID == "" || s.SubnetID == "" || s.RouteTableID == "" || s.OwnerTag == "" || s.OwnerValue == "" {
		return fmt.Errorf("complete AWS attachment scope and guest interfaces required")
	}
	// Journals and receipts require fresh API reads for CAS and convergence.
	c, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return err
	}
	api, err := provider.NewEC2Transport(credentials)
	if err != nil {
		return err
	}
	resolver := provider.Resolver{Client: c, API: api, Scope: s, Namespace: namespace, ClusterName: cfg.ClusterName, GroupID: cfg.GroupID, APIPort: uint16(port)}
	guest, err := provider.NewSSMGuestReader(credentials, resolver)
	if err != nil {
		return err
	}
	resources := provider.ForwardingResources{Scope: s,
		Routes:  &provider.Routes{API: api, Scope: s, Journal: provider.ConfigMapJournal{Client: c, Namespace: namespace}},
		Checks:  &provider.SourceChecks{API: api, Scope: s, Journal: provider.ConfigMapCheckJournal{Client: c, Namespace: namespace}},
		Ingress: &provider.Ingress{API: api, Scope: s, Journal: provider.ConfigMapIngressJournal{Client: c, Namespace: namespace}},
	}
	lifecycle := attachment.Reconciler{Store: attachment.ConfigMapStore{Client: c, Namespace: namespace},
		Lifetime:  CAPILifetime{Client: c, Namespace: namespace, ClusterName: cfg.ClusterName, MeshName: meshName},
		Forwarder: provider.AttachmentForwarder{Resolver: resolver, Bound: provider.BoundResources{Store: provider.ConfigMapBindingStore{Client: c, Namespace: namespace}, Resources: resources}, Guest: gatewayGuest{reader: guest, native: cfg.NativeInterface, tunnel: cfg.TunnelInterface}},
		Publication: attachment.GatewayPublication{
			Store:       attachment.ConfigMapPublicationStore{Client: c, Namespace: namespace},
			Resolver:    attachment.MeshConsumerResolver{Reader: c, Namespace: namespace, SecretName: meshName, SecretUID: cfg.MeshUID},
			Projections: attachment.ProjectionStore{Client: c, Namespace: namespace, Name: meshName, SecretUID: cfg.MeshUID},
			Verifier:    attachment.ConsumerVerifier{Reader: c},
			Transport:   attachment.CalicoTransport{Client: c, Observer: attachment.WindowsCalicoObserver{Guest: guest}}, APIVIP: apiVIP, APIPort: apiPort,
		},
	}
	return ctrl.NewControllerManagedBy(mgr).Named("aws-gateway-attachments").For(&corev1.ConfigMap{}).Complete(&RequestController{Client: c, Namespace: namespace, MeshName: meshName, Lifecycle: lifecycle})
}
