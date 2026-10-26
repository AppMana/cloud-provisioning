// Package aws is the AWS platform's provider-native pod network: the AWS
// VPC CNI on EC2 nodes.
package aws

import (
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni/awspod"
)

// VPCCNI is the AWS VPC CNI. Linux nodes run aws-node; Windows nodes run
// the Windows VPC CNI plugins with the VPC resource controller's IPAM,
// which AWS provides only for amd64.
type VPCCNI struct{}

// Name is the node class network name and node label value.
func (VPCCNI) Name() string { return awspod.NodeValue }

func (VPCCNI) Supports(os, arch string) error {
	switch {
	case os == "linux" && (arch == "" || arch == "amd64" || arch == "arm64"):
		return nil
	case os == "windows" && (arch == "" || arch == "amd64"):
		return nil
	}
	return fmt.Errorf("the AWS VPC CNI does not run %s/%s nodes", os, arch)
}
