package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Admission-only tests: no kernel mutations, API authentication or lifecycle
// continuity is exercised. Every changed identity must reject before cleanup;
// explicit and machine-file adoption sources must resolve identically.
func TestCloudRetirementConsumer(t *testing.T) {
	for _, fault := range []string{"none", "machine-file", "source", "missing-file", "site", "node-name", "node-uid", "machine", "secret", "key", "allocation", "missing", "duplicate"} {
		t.Run(fault, func(t *testing.T) {
			root, _ := wgtypes.GeneratePrivateKey()
			site, _ := wgtypes.GeneratePrivateKey()
			plan := receiveRestoreTestPlan(t, root, site)
			cfg := config{nodeName: plan.CloudNodeName, peersSecretName: plan.AdoptionName}
			consumer := attachment.ConsumerTarget{NodeName: plan.CloudNodeName, NodeUID: plan.CloudNodeUID, MachineName: plan.MachineName, SecretUID: plan.AdoptionUID, PublicKey: plan.RootPublicKey, TunnelAddress: "172.29.0.1"}
			switch fault {
			case "machine-file", "missing-file":
				cfg.peersSecretName = ""
				cfg.machineNameFile = filepath.Join(t.TempDir(), "machine")
				if fault == "machine-file" {
					if err := os.WriteFile(cfg.machineNameFile, []byte(plan.MachineName+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "source":
				cfg.peersSecretName = "other"
			case "site":
				consumer.Site = true
			case "node-name":
				consumer.NodeName = "other"
			case "node-uid":
				consumer.NodeUID = "other"
			case "machine":
				consumer.MachineName = "other"
			case "secret":
				consumer.SecretUID = "other"
			case "key":
				consumer.PublicKey = site.PublicKey().String()
			case "allocation":
				consumer.TunnelAddress = "172.29.0.99"
			}
			admitted := admittedReceiveRetirement{NodeUID: plan.CloudNodeUID}
			admitted.Withdrawal.Consumers = []attachment.ConsumerTarget{consumer}
			if fault == "missing" {
				admitted.Withdrawal.Consumers = nil
			}
			if fault == "duplicate" {
				admitted.Withdrawal.Consumers = append(admitted.Withdrawal.Consumers, consumer)
			}
			err := validateCloudRetirementConsumer(cfg, plan, admitted)
			if fault == "none" || fault == "machine-file" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("changed consumer authorized cleanup")
			}
		})
	}
}
