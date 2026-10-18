package attachment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// Batch binding and freshness only. These fixtures do not authenticate tokens,
// prove native cleanup, persist completion or authorize reservation release.
func TestReceiveRetirementObservationBatch(t *testing.T) {
	now := time.Now()
	for _, fault := range []string{"none", "missing", "duplicate", "unknown", "digest", "phase-key", "peer-key", "device", "boot", "container", "credential", "stale", "future", "age", "zero-scope"} {
		t.Run(fault, func(t *testing.T) {
			scope := ReceiveRetirementReceiptScope{digest: strings.Repeat("a", 64), nodes: map[string]string{"one": "node-one", "two": "node-two"}}
			var batch []handover.PodReceipt
			for _, node := range []string{"one", "two"} {
				batch = append(batch, handover.PodReceipt{BearerToken: "test-only", BootID: "boot", Receipt: handover.Receipt{NodeUID: node, AgentContainerID: "container", RoundHash: scope.digest, ObservedAt: now.Add(-time.Second)}})
			}
			age := time.Minute
			switch fault {
			case "missing":
				batch = batch[:1]
			case "duplicate":
				batch[1] = batch[0]
			case "unknown":
				batch[1].Receipt.NodeUID = "replacement"
			case "digest":
				batch[0].Receipt.RoundHash = strings.Repeat("b", 64)
			case "phase-key":
				batch[0].Receipt.FromKey = "old-key"
			case "peer-key":
				batch[0].Receipt.ToPeerKey = "peer-key"
			case "device":
				batch[0].Receipt.DeviceID = "phase-device"
			case "boot":
				batch[0].BootID = ""
			case "container":
				batch[0].Receipt.AgentContainerID = ""
			case "credential":
				batch[0].BearerToken = ""
			case "stale":
				batch[0].Receipt.ObservedAt = now.Add(-2 * time.Minute)
			case "future":
				batch[0].Receipt.ObservedAt = now.Add(time.Second)
			case "age":
				age = 0
			case "zero-scope":
				scope = ReceiveRetirementReceiptScope{}
			}
			err := scope.validateSubmissions(batch, now, age)
			if fault == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if err := scope.validateSubmissions(batch, now.Add(2*time.Minute), age); err == nil {
					t.Fatal("authentication delay extended freshness")
				}
			} else if err == nil {
				t.Fatal("invalid batch accepted")
			}
			// Shape checks cannot substitute for live credential authentication.
			if _, err := scope.AuthenticateComplete(context.Background(), batch, nil, age); err == nil {
				t.Fatal("missing trusted policies authenticated completion")
			}
		})
	}
}
