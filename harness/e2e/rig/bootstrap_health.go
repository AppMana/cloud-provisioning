package rig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// BootstrapHealth is optional: machine adapters provide it only when they can
// observe a terminal bootstrap failure without changing provisioning state.
// A nil result means no terminal failure was established, not bootstrap success.
type BootstrapHealth interface {
	BootstrapFailure(context.Context) error
}

// CloudInitFailure observes Linux cloud-init through the node's own transport.
// Unknown status and transport failures are inconclusive. Never include the
// error payload: cloud-init errors can contain userdata and credentials.
func CloudInitFailure(ctx context.Context, node Node) error {
	status, _ := cloudInitStatus(ctx, node)
	return cloudInitTerminalFailure(node.Name(), status)
}

func cloudInitStatus(ctx context.Context, node Node) (string, bool) {
	raw, err := node.Exec(ctx, "cloud-init", "status", "--format", "json")
	var exit *ExitError
	if err != nil && !errors.As(err, &exit) {
		return "", false
	}
	var status struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &status) != nil {
		return "", false
	}
	return status.Status, err == nil
}

func cloudInitTerminalFailure(name, status string) error {
	if status != "error" {
		return nil
	}
	return fmt.Errorf("%s: cloud-init reported a terminal bootstrap error; inspect private cloud-init logs", name)
}

// BootstrapCompletion is a guest-specific observation of completed first boot.
// It is independent of infrastructure readiness and Kubernetes registration.
type BootstrapCompletion interface {
	BootstrapComplete(context.Context) (bool, error)
}

// CloudInitComplete requires both the native completed status and CAPI's
// success sentinel. Running, disabled, malformed, and unreachable are not done.
func CloudInitComplete(ctx context.Context, node Node) (bool, error) {
	status, cleanExit := cloudInitStatus(ctx, node)
	if err := cloudInitTerminalFailure(node.Name(), status); err != nil {
		return false, err
	}
	if status != "done" || !cleanExit {
		return false, nil
	}
	_, err := node.Exec(ctx, "test", "-f", "/run/cluster-api/bootstrap-success.complete")
	return err == nil, nil
}

// WaitBootstrap never treats an unavailable observer as evidence of success.
func WaitBootstrap(ctx context.Context, node Node, within time.Duration) error {
	observer, ok := node.(BootstrapCompletion)
	if !ok {
		return fmt.Errorf("%s: completed bootstrap observation is unsupported", node.Name())
	}
	return wait.Until(ctx, within, node.Name()+" bootstrap did not complete", func(ctx context.Context) error {
		complete, err := observer.BootstrapComplete(ctx)
		if err != nil {
			return wait.Fatal(err)
		}
		if !complete {
			return fmt.Errorf("waiting for completed bootstrap and CAPI success marker")
		}
		return nil
	})
}
