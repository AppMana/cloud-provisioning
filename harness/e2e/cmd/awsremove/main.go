// awsremove verifies product cleanup and CAPA-owned instance termination.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/claim"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/aws"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	work := flag.String("work-dir", "", "private AWS run directory")
	site := flag.String("site-work-dir", "", "existing single-NIC VM site")
	apiPort := flag.Int("api-port", 6443, "distribution API port (MicroK8s: 16443)")
	name := flag.String("claim", "", "CAPA claim to delete")
	report := flag.String("report", "", "new removal evidence JSON path")
	flag.Parse()
	if *work == "" || *site == "" || *name == "" || *report == "" {
		return fmt.Errorf("all flags are required")
	}
	if *apiPort < 1 || *apiPort > 65535 {
		return fmt.Errorf("api-port must be between 1 and 65535")
	}
	if _, err := os.Stat(*report); !os.IsNotExist(err) {
		return fmt.Errorf("report exists or cannot be checked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	raw, err := os.ReadFile(filepath.Join(*work, "resources.json"))
	if err != nil {
		return err
	}
	var state struct {
		Region, RunID, VPCID string
		CleanedUp            bool
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.CleanedUp {
		return fmt.Errorf("run already cleaned up")
	}
	topo := lab.Default()
	siteRig := vm.New(topo, *site)
	k := &kube.Client{Bastion: siteRig.Node("bastion"), APIPort: *apiPort, ControlPlanes: cluster.ControlPlaneAddresses(topo)}
	node, err := k.Get(ctx, claim.Namespace, "machine", *name, "{.status.nodeRef.name}")
	if err != nil {
		return err
	}
	if node == "" {
		return fmt.Errorf("Machine has no associated Node")
	}
	if err = k.WaitMachineAssociation(ctx, claim.Namespace, *name, node); err != nil {
		return err
	}
	uid, err := k.Get(ctx, "", "node", node, "{.metadata.uid}")
	if err != nil {
		return err
	}
	providerID, err := k.Get(ctx, claim.Namespace, "machine", *name, "{.spec.providerID}")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(providerID, "aws:///") {
		return fmt.Errorf("not an AWS Machine")
	}
	parts := strings.Split(providerID, "/")
	id := parts[len(parts)-1]
	clusterName, err := k.Get(ctx, claim.Namespace, "machine", *name, "{.spec.clusterName}")
	if err != nil {
		return err
	}
	if clusterName != state.RunID {
		return fmt.Errorf("Machine does not belong to this run")
	}
	cli := &aws.CLI{Region: state.Region, SessionPath: filepath.Join(*work, "harness-session.json")}
	description, err := cli.Call(ctx, "ec2", "describe-instances", map[string]any{"InstanceIds": []string{id}})
	if err != nil {
		return err
	}
	var ownership struct {
		Reservations []struct {
			Instances []struct {
				VPCID string `json:"VpcId"`
				Tags  []struct{ Key, Value string }
			}
		}
	}
	if err = json.Unmarshal(description, &ownership); err != nil {
		return err
	}
	if len(ownership.Reservations) != 1 || len(ownership.Reservations[0].Instances) != 1 {
		return fmt.Errorf("ambiguous instance ownership")
	}
	instance := ownership.Reservations[0].Instances[0]
	owned := false
	for _, tag := range instance.Tags {
		if tag.Key == "cloud-provisioning-test" && tag.Value == state.RunID {
			owned = true
		}
	}
	if !owned || instance.VPCID != state.VPCID {
		return fmt.Errorf("instance ownership does not match this test VPC and run")
	}
	fmt.Println("Deleting claim", *name, "with instance", id, "and Node", node)
	if err = (&claim.Remover{Kube: k}).Delete(ctx, *name, node); err != nil {
		return err
	}
	err = wait.Until(ctx, 5*time.Minute, "CAPA did not terminate "+id, func(ctx context.Context) error {
		raw, err := cli.Call(ctx, "ec2", "describe-instances", map[string]any{"InstanceIds": []string{id}})
		if err != nil {
			return err
		}
		var result struct {
			Reservations []struct {
				Instances []struct{ State struct{ Name string } }
			}
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if len(result.Reservations) != 1 || len(result.Reservations[0].Instances) != 1 || result.Reservations[0].Instances[0].State.Name != "terminated" {
			return fmt.Errorf("instance has not terminated")
		}
		return nil
	})
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(map[string]any{"claim": *name, "node": node, "nodeUID": uid, "instanceID": id, "providerID": providerID, "terminated": true, "productCleanup": true, "observedAt": time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(*report, body, 0600); err != nil {
		return err
	}
	fmt.Println("Verified instance termination and removal of claim, Machine, infrastructure, Node, credentials and peer fields")
	return nil
}
