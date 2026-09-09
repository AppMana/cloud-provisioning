// vmprovider runs the fake CAPI infrastructure lifecycle on an existing VM lab.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/install"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/provider"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm"
)

func main() {
	work := flag.String("work-dir", "", "existing VM site work directory")
	namespace := flag.String("namespace", "cloud-provisioning", "infrastructure Machine namespace")
	slots := flag.Int("remote-slots", 2, "remote capacity used when creating this lab (2..32)")
	port := flag.Int("api-port", 6443, "distribution API port")
	once := flag.Bool("once", false, "perform one provider reconciliation pass")
	flag.Parse()
	if err := run(*work, *namespace, *slots, *port, *once); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(work, namespace string, count, port int, once bool) error {
	if work == "" || namespace == "" || port < 1 || port > 65535 {
		return fmt.Errorf("existing work directory, namespace and valid API port required")
	}
	info, err := os.Stat(work)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("existing work directory required")
	}
	topo, err := lab.WithRemoteSlots(count)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "cloud-provisioning-cldt.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another lab operation holds the provider lock: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	binary := filepath.Join(work, "binaries", "wg-dialer-linux-amd64")
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("existing harness dialer binary required at %s", binary)
	}
	origin := &install.Origin{Path: binary}
	if err := origin.Start(ctx); err != nil {
		return err
	}
	defer origin.Stop(context.Background())
	r := vm.New(topo, work)
	k := &kube.Client{Bastion: r.Node("bastion"), APIPort: port, ControlPlanes: cluster.ControlPlaneAddresses(topo)}
	var names []string
	for _, node := range topo.Nodes {
		if node.Role == lab.Remote {
			names = append(names, node.Name)
		}
	}
	store := &provider.SlotStore{API: k, Namespace: namespace, Name: "cldt-vm-slots", Lab: topo.Name, Slots: names}
	c := &provider.Controller{Kube: k, Rig: r, Topology: topo, LabName: topo.Name, Slots: store}
	for {
		pass, stop := context.WithTimeout(ctx, provider.ReconcileTimeout)
		observed, err := c.Reconcile(pass, namespace)
		stop()
		if err != nil {
			if once {
				return err
			}
			fmt.Fprintln(os.Stderr, "provider pass:", err)
		} else {
			if err := json.NewEncoder(os.Stdout).Encode(observed); err != nil {
				return err
			}
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}
