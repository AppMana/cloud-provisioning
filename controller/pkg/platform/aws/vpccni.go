// Package aws is the AWS platform's provider-native pod network: the AWS
// VPC CNI on EC2 nodes.
package aws

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni/awspod"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// VPCCNI is the AWS VPC CNI. Linux nodes run aws-node; Windows nodes run
// the Windows VPC CNI plugins with the VPC resource controller's IPAM,
// which AWS provides only for amd64.
type VPCCNI struct {
	// Windows is whether this cluster runs Windows IPAM, without which
	// Windows nodes' pods get no addresses.
	Windows bool
}

// Name is the node class network name and node label value.
func (VPCCNI) Name() string { return awspod.NodeValue }

// VerifyNode rejects a VPC CNI node that retains site Calico BGP addresses.
func (VPCCNI) VerifyNode(node *corev1.Node) error { return awspod.ValidateNodeCNI(node) }

func (v VPCCNI) Supports(os, arch string) error {
	switch {
	case os == "linux" && (arch == "" || arch == "amd64" || arch == "arm64"):
		return nil
	case os == "windows" && (arch == "" || arch == "amd64"):
		if !v.Windows {
			return fmt.Errorf("Windows nodes need Windows IPAM, which runs when platforms.aws.vpcCNI.windows.ipamImage names the patched VPC resource controller")
		}
		return nil
	}
	return fmt.Errorf("the AWS VPC CNI does not run %s/%s nodes", os, arch)
}

// Observer reports the VPC CNI's allocations from the node's ENIs.
type Observer struct{ *awspod.Observer }

func (o Observer) Allocations(ctx context.Context, machine *unstructured.Unstructured, nodeName string) (platform.Allocation, error) {
	a, err := o.AllocationsFor(ctx, machine, nodeName)
	if err != nil {
		return platform.Allocation{}, err
	}
	scope := a.Scope
	return platform.Allocation{
		PodPrefixes:    a.PodPrefixes,
		NetworkID:      fmt.Sprintf("aws:%s:%s:%s", scope.Account, scope.Region, scope.VPC),
		NativePrefixes: a.NativePrefixes,
	}, nil
}
