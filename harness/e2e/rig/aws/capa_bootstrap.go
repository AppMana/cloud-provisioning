package aws

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// CAPALinuxCommands observes CAPA's secure MIME bootstrap contract. Select it
// only for CAPA-owned Linux workers, not arbitrary EC2 image-build instances.
type CAPALinuxCommands struct{ LinuxCommands }

const capaIncludeWarning = "[Errno 2] No such file or directory: '/etc/secret-userdata.txt' for url: file:///etc/secret-userdata.txt"

// CAPA v2.12.1 documents this initial include warning in userdata-privacy.md.
// cloud-init 26.1 reports it as exit 2 even after successful final modules.
func (CAPALinuxCommands) BootstrapComplete(ctx context.Context, node rig.Node) (bool, error) {
	complete, err := rig.CloudInitComplete(ctx, node)
	if complete || err != nil {
		return complete, err
	}
	raw, err := node.Exec(ctx, "cloud-init", "status", "--format", "json")
	var exited *rig.ExitError
	if !errors.As(err, &exited) || exited.Code != 2 || !onlyCAPAIncludeWarning(raw) {
		return false, nil
	}
	_, err = node.Exec(ctx, "test", "-f", "/run/cluster-api/bootstrap-success.complete")
	return err == nil, nil
}

func onlyCAPAIncludeWarning(raw []byte) bool {
	type issues struct {
		Errors      []json.RawMessage   `json:"errors"`
		Recoverable map[string][]string `json:"recoverable_errors"`
	}
	var report struct {
		issues
		Status   string `json:"status"`
		Extended string `json:"extended_status"`
	}
	if json.Unmarshal(raw, &report) != nil || report.Status != "done" || report.Extended != "degraded done" || report.Errors == nil || len(report.Errors) != 0 || len(report.Recoverable) != 1 {
		return false
	}
	warnings := report.Recoverable["WARNING"]
	if len(warnings) == 0 {
		return false
	}
	for _, warning := range warnings {
		if warning != capaIncludeWarning {
			return false
		}
	}
	// Check the per-stage reports too; a malformed or inconsistent aggregate
	// must not conceal a module error.
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, stage := range []string{"init-local", "init", "modules-config", "modules-final"} {
		var detail issues
		if len(fields[stage]) == 0 || string(fields[stage]) == "null" || json.Unmarshal(fields[stage], &detail) != nil || detail.Errors == nil || len(detail.Errors) != 0 || detail.Recoverable == nil {
			return false
		}
		for level, messages := range detail.Recoverable {
			for _, message := range messages {
				if level != "WARNING" || message != capaIncludeWarning {
					return false
				}
			}
		}
	}
	return true
}
