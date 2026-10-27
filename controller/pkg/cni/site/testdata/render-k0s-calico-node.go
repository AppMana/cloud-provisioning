//go:build ignore

// Renders k0s's bundled calico-node manifest the way k0s does, for the
// values README.md lists:
//
//	go run render-k0s-calico-node.go $(go env GOMODCACHE)/github.com/k0sproject/k0s@v1.36.5-0.20260918181138-c37fe960bdee/static/manifests/calico/DaemonSet/calico-node.yaml | grep -v '^\s*$' > k0s-v1.36.5-calico-node.yaml
package main

import (
	"os"
	"text/template"
)

func main() {
	t := template.Must(template.ParseFiles(os.Args[1]))
	values := map[string]any{
		"CalicoCNIImage": "quay.io/k0sproject/calico-cni:v3.32.1-0", "CalicoNodeImage": "quay.io/k0sproject/calico-node:v3.32.1-0",
		"PullPolicy": "IfNotPresent", "MTU": 1450, "EnableIPv4": true, "EnableIPv6": true, "Mode": "bird", "Overlay": "Never",
		"IPAutodetectionMethod": "cidr=10.10.0.0/24", "IPV6AutodetectionMethod": "cidr=fd00:10::/64",
		"VxlanPort": 4789, "VxlanVNI": 4096, "EnableWireguard": false,
		"ClusterCIDRIPv4": "10.244.0.0/16", "ClusterCIDRIPv6": "fd00:244::/56", "EnvVars": map[string]string{},
	}
	if err := t.Execute(os.Stdout, values); err != nil {
		panic(err)
	}
}
