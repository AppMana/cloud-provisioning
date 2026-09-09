// Command k0segress renders the Linux default-route addon from a captured native
// DaemonSet. Rendering does not establish reachability or mutate a cluster.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster/k0s"
)

func main() {
	native := flag.String("native-daemonset", "", "captured kube-system/konnectivity-agent JSON")
	hosts := flag.String("nodes", "", "comma-separated verified Linux hostname labels (at least two)")
	output := flag.String("output", "", "new manifest file; never overwritten")
	flag.Parse()
	if err := render(*native, *hosts, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func render(native, hosts, output string) error {
	if native == "" || hosts == "" || output == "" {
		return fmt.Errorf("--native-daemonset, --nodes and --output required")
	}
	raw, err := os.ReadFile(native)
	if err != nil {
		return err
	}
	manifest, err := k0s.DefaultRouteAgents(raw, strings.Split(hosts, ","))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(manifest, '\n'))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
