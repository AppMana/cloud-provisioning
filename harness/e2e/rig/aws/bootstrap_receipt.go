package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Query only the secure consumer's non-secret receipt. EC2Launch success is
// insufficient because this consumer runs as a detached task.
const windowsBootstrapReceiptQuery = `$ErrorActionPreference='Stop'
$root='C:\ProgramData\CloudProvisioning\Bootstrap'
$path=Join-Path $root 'status.json'
if (!(Test-Path -LiteralPath $path)) { exit 0 }
if (((Get-Item -LiteralPath $root).Attributes -band [IO.FileAttributes]::ReparsePoint) -or ((Get-Item -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint)) { exit 0 }
$r=Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
@{schemaVersion=$r.schemaVersion;instanceID=$r.instanceID;bootstrapID=$r.bootstrapID;state=$r.state;stage=$r.stage;updatedAt=$r.updatedAt;protectedRoot=(Get-Acl $root).AreAccessRulesProtected;payloadPresent=(Test-Path -LiteralPath (Join-Path $root 'join.ps1'))} | ConvertTo-Json -Compress`

type bootstrapReceipt struct {
	SchemaVersion  int    `json:"schemaVersion"`
	InstanceID     string `json:"instanceID"`
	BootstrapID    string `json:"bootstrapID"`
	State          string `json:"state"`
	Stage          string `json:"stage"`
	UpdatedAt      string `json:"updatedAt"`
	ProtectedRoot  bool   `json:"protectedRoot"`
	PayloadPresent *bool  `json:"payloadPresent"`
}

var bootstrapDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func receiptResult(raw []byte, instanceID string) (bool, error) {
	var r bootstrapReceipt
	if json.Unmarshal(raw, &r) != nil || r.SchemaVersion != 1 || r.PayloadPresent == nil || !r.ProtectedRoot || instanceID == "" || r.InstanceID != instanceID || !bootstrapDigest.MatchString(r.BootstrapID) {
		return false, nil
	}
	if _, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err != nil {
		return false, nil
	}
	switch r.State {
	case "failed":
		return false, fmt.Errorf("Windows secure-bootstrap consumer reported a terminal failure; inspect its protected receipt")
	case "succeeded":
		return r.Stage == "complete" && !*r.PayloadPresent, nil
	default:
		return false, nil
	}
}

func windowsBootstrapResult(ctx context.Context, node rig.Node) (bool, error) {
	identity, ok := node.(interface{ InstanceIdentity() string })
	if !ok || identity.InstanceIdentity() == "" {
		return false, nil
	}
	raw, err := node.Exec(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsBootstrapReceiptQuery)
	if err != nil {
		return false, nil
	}
	return receiptResult(raw, identity.InstanceIdentity())
}
