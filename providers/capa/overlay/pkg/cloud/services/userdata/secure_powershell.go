package userdata

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
)

//go:embed secure_windows.ps1
var windowsSecretsConsumer []byte

// WindowsSecretsManagerUserData retains CAPA's encrypted chunks and cleanup
// ownership while using EC2Launch instead of a Linux cloud-init boothook.
func WindowsSecretsManagerUserData(prefix string, chunks int32, region string) ([]byte, error) {
	if !regexp.MustCompile(`^aws\.cluster\.x-k8s\.io/[a-zA-Z0-9/-]+$`).MatchString(prefix) || chunks < 1 || chunks > 1024 || !regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`).MatchString(region) {
		return nil, fmt.Errorf("invalid Windows bootstrap secret reference")
	}
	settings, _ := json.Marshal(map[string]any{"prefix": prefix, "chunks": chunks, "region": region})
	script := bytes.ReplaceAll(windowsSecretsConsumer, []byte("__SETTINGS__"), []byte(base64.StdEncoding.EncodeToString(settings)))
	// EC2Launch extracts PowerShell literally. All dynamic data is base64 JSON;
	// the original credential-bearing script is fetched only inside the guest.
	// EC2Launch v2 starts SSM after inline userdata. A slow or stuck join must
	// not prevent out-of-band diagnosis. Detach preserves one-shot execution;
	// CAPI observes the Node association, not the launch agent's completion.
	return append(append([]byte("<powershell>\n"), script...), []byte("\n</powershell>\n<persist>false</persist>\n<detach>true</detach>\n")...), nil
}
