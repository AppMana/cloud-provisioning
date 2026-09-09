// importsite supplies CAPI's imported-control-plane contract from the live site.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/provider"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm"
)

func main() {
	name := flag.String("name", "", "CAPI cluster name")
	namespace := flag.String("namespace", "cloud-provisioning", "CAPI namespace")
	work := flag.String("work-dir", "", "existing VM site work directory")
	port := flag.Int("api-port", 6443, "distribution API port")
	flag.Parse()
	if *name == "" || *work == "" {
		fmt.Fprintln(os.Stderr, "-name and -work-dir are required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := vm.New(lab.Default(), *work)
	k := &kube.Client{Bastion: r.Node("bastion"), APIPort: *port, ControlPlanes: cluster.ControlPlaneAddresses(lab.Default())}
	obj := map[string]any{"apiVersion": "containernet.appmana.com/v1beta2", "kind": "ImportedControlPlane", "metadata": map[string]string{"name": *name, "namespace": *namespace}, "spec": map[string]any{}}
	raw, _ := json.Marshal(obj)
	if err := k.Apply(ctx, raw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	c := &provider.Controller{Kube: k}
	if err := c.ImportControlPlane(ctx, *namespace, *name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Observed and imported site control plane for", *name)
}
