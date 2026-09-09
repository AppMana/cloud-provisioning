// awsrow measures CAPA-owned EC2 workers against an existing single-NIC site.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	adopt "github.com/appmana/cloud-provisioning/harness/e2e/adoption"
	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/k0s"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/microk8s"
	"github.com/appmana/cloud-provisioning/harness/e2e/install"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
	"github.com/appmana/cloud-provisioning/harness/e2e/observe"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
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
	expectedImage := flag.String("expected-image", "", "authorized Linux image expected on each measured worker; defaults to the prepared AMI")
	site := flag.String("site-work-dir", "", "existing single-NIC VM site directory")
	distro := flag.String("distro", "k0s", "site distribution: k0s or microk8s")
	cni := flag.String("cni", "default", "distribution-supported site network")
	claims := flag.String("claims", "aws-k0s-1", "existing CAPA claim/Machine names, comma separated")
	placementName := flag.String("placement", "", "change tunnel endpoints: control-plane, one-worker, two-workers, all-nodes")
	report := flag.String("report-dir", "", "evidence directory (must not exist)")
	timeout := flag.Duration("timeout", 45*time.Minute, "row deadline")
	flag.Parse()
	if *work == "" || *site == "" {
		return fmt.Errorf("-work-dir and -site-work-dir are required")
	}
	builder, profile, err := workerProfile(*distro, *cni)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	raw, err := os.ReadFile(filepath.Join(*work, "resources.json"))
	if err != nil {
		return err
	}
	var state struct {
		Region, AssetBucket, PreparedAMIID string
		AuthorizedLinuxAMIs                []string
		CleanedUp                          bool
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.CleanedUp || state.PreparedAMIID == "" {
		return fmt.Errorf("run needs an active, prepared AWS image")
	}
	imageID, err := selectedWorkerImage(state.PreparedAMIID, state.AuthorizedLinuxAMIs, *expectedImage)
	if err != nil {
		return err
	}
	topo := lab.Default()
	siteRig := vm.New(topo, *site)
	k := &kube.Client{Bastion: siteRig.Node("bastion"), ControlPlanes: cluster.ControlPlaneAddresses(topo)}
	if err := builder.Reuse(ctx, cluster.Deps{Rig: siteRig, Kube: k, Topology: topo, Network: profile.BuilderNetwork, WorkDir: *site}); err != nil {
		return err
	}
	cli := &aws.CLI{Region: state.Region, SessionPath: filepath.Join(*work, "harness-session.json")}
	fleet := &rig.Fleet{Site: siteRig, Remotes: map[string]rig.Node{}}
	var nodes []string
	for _, n := range cluster.SiteNodes(topo) {
		nodes = append(nodes, n.Name)
	}
	imageRaw, err := k.Run(ctx, "-n", "cloud-provisioning", "get", "daemonset", "cloud-provisioning-dialer", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		return err
	}
	image := strings.TrimSpace(string(imageRaw))
	if image == "" {
		return fmt.Errorf("site dialer has no image")
	}
	archive, err := os.CreateTemp(*work, "dialer-image-*.tar")
	if err != nil {
		return err
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	remoteRaw, err := k.Run(ctx, "-n", "cloud-provisioning", "get", "daemonset", "cloud-provisioning-dialer-remote", "-o", "json")
	if err != nil {
		archive.Close()
		return err
	}
	remoteDS, err := adopt.DecodeDaemonSet(remoteRaw)
	if err != nil {
		archive.Close()
		return err
	}
	images := adopt.RequiredImages(image, remoteDS)
	save := exec.CommandContext(ctx, "docker", append([]string{"save"}, images...)...)
	save.Stdout = archive
	if err = save.Run(); err != nil {
		archive.Close()
		return fmt.Errorf("saving dialer image: %w", err)
	}
	if err = archive.Close(); err != nil {
		return err
	}
	bindings := []map[string]any{}
	for _, name := range strings.Split(*claims, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("empty claim name")
		}
		fmt.Println("Observing CAPA machine", name)
		var providerID string
		err = wait.Until(ctx, 10*time.Minute, "CAPA did not launch "+name, func(ctx context.Context) error {
			out, e := k.Get(ctx, "cloud-provisioning", "machine", name, "{.spec.providerID}")
			providerID = out
			if e != nil {
				return e
			}
			if !strings.HasPrefix(providerID, "aws:///") {
				return fmt.Errorf("AWS providerID is not yet published")
			}
			return nil
		})
		if err != nil {
			return err
		}
		parts := strings.Split(providerID, "/")
		id := parts[len(parts)-1]
		if !strings.HasPrefix(id, "i-") {
			return fmt.Errorf("invalid EC2 providerID")
		}
		remote := &aws.Node{API: cli, Input: &aws.S3Input{CLI: cli, Bucket: state.AssetBucket}, NodeName: name, InstanceID: id, Commands: aws.CAPALinuxCommands{}}
		fmt.Println("Waiting for native", builder.Name(), "runtime on", id)
		err = wait.Until(ctx, 15*time.Minute, builder.Name()+" did not bootstrap on "+name, func(ctx context.Context) error {
			if err := builder.VerifyWorker(ctx, remote); err != nil {
				if failed := remote.BootstrapFailure(ctx); failed != nil {
					return wait.Fatal(failed)
				}
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Println("Waiting for completed bootstrap on", id)
		if err := rig.WaitBootstrap(ctx, remote, 15*time.Minute); err != nil {
			return err
		}
		fmt.Println("Native bootstrap and CAPI success marker verified on", id)
		f, e := os.Open(archivePath)
		if e != nil {
			return e
		}
		_, e = remote.Pipe(ctx, f, builder.ImportArgs()...)
		f.Close()
		if e != nil {
			return fmt.Errorf("importing dialer measurement image: %w", e)
		}
		var nodeName string
		err = wait.Until(ctx, 10*time.Minute, "CAPI has no Node reference for "+name, func(ctx context.Context) error {
			out, e := k.Get(ctx, "cloud-provisioning", "machine", name, "{.status.nodeRef.name}")
			nodeName = out
			if e != nil {
				return e
			}
			if nodeName == "" {
				return fmt.Errorf("no Node reference")
			}
			return nil
		})
		if err != nil {
			return err
		}
		remote.NodeName = nodeName
		fleet.Remotes[nodeName] = remote
		nodes = append(nodes, nodeName)
		if err = k.WaitMachineAssociation(ctx, "cloud-provisioning", name, nodeName); err != nil {
			return err
		}
		nodeRaw, e := k.Run(ctx, "get", "node", nodeName, "-o", "json")
		if e != nil {
			return e
		}
		var registered struct {
			Metadata struct{ Labels map[string]string }
			Spec     struct {
				Taints []struct{ Key, Effect string }
			}
		}
		if e = json.Unmarshal(nodeRaw, &registered); e != nil {
			return e
		}
		tainted := false
		for _, t := range registered.Spec.Taints {
			if t.Key == "cloud-provisioning.appmana.com/internet-facing" && t.Effect == "NoSchedule" {
				tainted = true
			}
		}
		if registered.Metadata.Labels["cloud-provisioning.appmana.com/role"] != "cloud-worker" || !tainted {
			return fmt.Errorf("AWS worker is missing its product label or taint")
		}
		if e := builder.VerifyWorker(ctx, remote); e != nil {
			return e
		}
		uid, e := k.Get(ctx, "", "node", nodeName, "{.metadata.uid}")
		if e != nil {
			return e
		}
		desc, e := cli.Call(ctx, "ec2", "describe-instances", map[string]any{"InstanceIds": []string{id}})
		if e != nil {
			return e
		}
		var ec2 struct {
			Reservations []struct {
				Instances []struct {
					ImageID           string `json:"ImageId"`
					NetworkInterfaces []json.RawMessage
				}
			}
		}
		if e = json.Unmarshal(desc, &ec2); e != nil {
			return e
		}
		if len(ec2.Reservations) != 1 || len(ec2.Reservations[0].Instances) != 1 {
			return fmt.Errorf("EC2 returned ambiguous instance identity")
		}
		instance := ec2.Reservations[0].Instances[0]
		if len(instance.NetworkInterfaces) != 1 || instance.ImageID != imageID {
			return fmt.Errorf("EC2 machine does not match single-ENI prepared-image contract")
		}
		physical, e := remote.Exec(ctx, "sh", "-ec", `for p in /sys/class/net/*; do [ ! -e "$p/device" ] || basename "$p"; done`)
		if e != nil {
			return e
		}
		interfaces := strings.Fields(string(physical))
		if len(interfaces) != 1 {
			return fmt.Errorf("guest has %d physical NICs", len(interfaces))
		}
		remote.NIC = interfaces[0]
		bindings = append(bindings, map[string]any{"machine": name, "node": nodeName, "nodeUID": uid, "providerID": providerID, "instanceID": id, "physicalNIC": remote.NIC, "eniCount": 1, "imageID": instance.ImageID})
		fmt.Println("CAPI associated", name, "with", nodeName, "on one ENI")
	}
	reportDir := filepath.Join(*work, "rows", time.Now().UTC().Format("20060102T150405Z"))
	if *report != "" {
		reportDir = *report
	}
	if _, err = os.Stat(reportDir); !os.IsNotExist(err) {
		return fmt.Errorf("evidence directory already exists or cannot be checked: %s", reportDir)
	}
	if err = os.MkdirAll(reportDir, 0700); err != nil {
		return err
	}
	bindingData, _ := json.MarshalIndent(bindings, "", "  ")
	if err = os.WriteFile(filepath.Join(reportDir, "bindings.json"), bindingData, 0600); err != nil {
		return err
	}
	// A passing workload matrix can coexist with a frozen bootstrap tunnel.
	// Require the actual remote adoption DaemonSet, not merely a Ready Node.
	recordAdoption := func() error {
		adoption := map[string]any{}
		for _, binding := range bindings {
			nodeName := binding["node"].(string)
			machine := binding["machine"].(string)
			observation, e := adopt.Wait(ctx, k, machine, nodeName, 5*time.Minute)
			err = e
			adoption[nodeName] = observation
			if err != nil {
				return err
			}
		}
		adoptionData, _ := json.MarshalIndent(adoption, "", "  ")
		if err = os.WriteFile(filepath.Join(reportDir, "adoption.json"), adoptionData, 0600); err != nil {
			return err
		}
		return nil
	}
	if err = recordAdoption(); err != nil {
		return err
	}
	if *placementName != "" {
		placement, err := install.PlacementNamed(*placementName)
		if err != nil {
			return err
		}
		fmt.Println("Moving tunnel endpoints to", placement.Name)
		_, err = k.Helm(ctx, "upgrade", "cloud-provisioning", "/tmp/chart", "--namespace", install.Namespace,
			"--reuse-values", "--wait", "--timeout", "6m", "--set-string", "tunnel.endpoints="+strings.ReplaceAll(placement.Endpoints, ",", `\,`))
		if err != nil {
			return fmt.Errorf("endpoint placement Helm upgrade failed")
		}
		product := &install.Product{Kube: k, Rig: siteRig, Topology: topo}
		if err = product.WaitPlacement(ctx, placement); err != nil {
			return err
		}
		if err = recordAdoption(); err != nil {
			return err
		}
		data, _ := json.Marshal(placement)
		if err = os.WriteFile(filepath.Join(reportDir, "placement.json"), data, 0600); err != nil {
			return err
		}
	}
	if err = cluster.EnsureCRICTL(ctx, fleet, *site, nodes); err != nil {
		return err
	}
	pods := &check.Pods{Kube: k, Rig: fleet, Namespace: check.UniqueNamespace(time.Now().UnixNano()), CRIEndpoint: builder.CRIEndpoint()}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		pods.Stop(cleanup)
	}()
	targets, err := pods.Start(ctx, nodes, 10*time.Minute)
	if err != nil {
		return err
	}
	if err = observe.Capture(ctx, reportDir, "ready", k, fleet, nodes); err != nil {
		return err
	}
	fmt.Println("Measuring pod, Service, large-transfer, DNS and external paths across", len(nodes), "nodes")
	journal, err := os.OpenFile(filepath.Join(reportDir, "matrix-attempts.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	matrix, recordErr := recordedMatrix(ctx, pods, targets, check.Options{Port: check.Port, ExternalURL: check.ExternalURL}, 10*time.Minute, 10*time.Second, journal)
	closeErr := journal.Close()
	if recordErr != nil {
		return recordErr
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Println(matrix.Report())
	detail := matrix.Details()
	detail["profile"] = profile
	detail["criEndpoint"] = builder.CRIEndpoint()
	data, _ := json.MarshalIndent(detail, "", "  ")
	if err = os.WriteFile(filepath.Join(reportDir, "matrix.json"), data, 0600); err != nil {
		return err
	}
	management := check.KubeletAccess(ctx, k, pods.Namespace, nodes)
	data, err = json.MarshalIndent(management, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(reportDir, "kubelet-access.json"), data, 0600); err != nil {
		return err
	}
	if len(management) == 0 {
		return fmt.Errorf("no kubelet management paths were checked")
	}
	for _, result := range management {
		if !result.OK {
			return fmt.Errorf("AWS kubelet management check failed; evidence in %s", reportDir)
		}
	}
	if !matrix.OK() {
		return fmt.Errorf("AWS matrix failed; evidence in %s", reportDir)
	}
	fmt.Println("AWS matrix passed; evidence in", reportDir)
	return nil
}

// workerSite keeps distribution behavior behind its existing builder contract.
// Guest execution and lifecycle remain the AWS rig's responsibility.
type workerSite interface {
	cluster.Builder
	cluster.SiteReuser
	cluster.WorkerVerifier
}

func workerProfile(distro, cni string) (workerSite, network.Profile, error) {
	profile, err := network.Select(distro, cni)
	if err != nil {
		return nil, profile, err
	}
	builder, err := cluster.For(distro)
	if err != nil {
		return nil, profile, err
	}
	site, ok := builder.(workerSite)
	if !ok {
		return nil, profile, fmt.Errorf("%s has no verified existing-site worker observer", distro)
	}
	return site, profile, nil
}

// Selection is independent of the run default, but must remain explicitly
// authorized. The row subsequently checks the actual EC2 instance image.
func selectedWorkerImage(prepared string, authorized []string, requested string) (string, error) {
	if requested == "" || requested == prepared {
		return prepared, nil
	}
	for _, image := range authorized {
		if image == requested {
			return requested, nil
		}
	}
	return "", fmt.Errorf("requested worker image is not authorized in this run")
}
