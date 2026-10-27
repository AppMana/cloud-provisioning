// Package windowscni holds the AWS VPC CNI's Windows node initializer: the
// HostProcess DaemonSet that writes the native plugin's configuration, its
// scripts, and the pinned recipe of the plugin build the image carries.
package windowscni

import _ "embed"

var (
	//go:embed cni-daemonset.yaml
	DaemonSet []byte
	//go:embed configure-cni.ps1
	Configure string
	//go:embed verify-cni.ps1
	Verify string
	// Recipe is the patched vpc-bridge build (providers/aws-vpc-cni-plugins)
	// Windows images bake: its archive, binaries, source and the features
	// (hostRouteCIDRs, podMTU) its configuration may use.
	//go:embed cni-artifacts.json
	Recipe string
)
