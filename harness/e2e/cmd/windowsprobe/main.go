package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/aws"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	workArg := flag.String("work-dir", "", "private AWS run directory with Windows probe instances")
	output := flag.String("output", "", "new result JSON path")
	render := flag.Bool("render-userdata", false, "write the native EC2Launch probe document without accessing AWS")
	prepare := flag.String("prepare-script", "", "optional image preparation PowerShell appended to rendered probe userdata")
	flag.Parse()
	if *prepare != "" && !*render {
		return fmt.Errorf("-prepare-script requires -render-userdata")
	}
	if *render {
		if *output == "" {
			return fmt.Errorf("-output is required")
		}
		if _, err := os.Stat(*output); !os.IsNotExist(err) {
			return fmt.Errorf("output must be a new file")
		}
		script := []byte(`$ErrorActionPreference = 'Stop'
$null = New-Item -ItemType Directory -Force -Path 'C:\ProgramData\CloudProvisioning'
[System.IO.File]::WriteAllText('C:\ProgramData\CloudProvisioning\bootstrap.txt', 'userdata & < > "quotes" Ω', (New-Object System.Text.UTF8Encoding($false)))
`)
		if *prepare != "" {
			body, err := os.ReadFile(*prepare)
			if err != nil {
				return err
			}
			script = append(script, body...)
		}
		data, err := (bootstrap.Data{Format: bootstrap.PowerShell, Value: script}).EC2Launch()
		if err != nil {
			return err
		}
		if len(data) > 16384 {
			return fmt.Errorf("EC2Launch document exceeds 16 KiB; stage preparation in the image builder")
		}
		f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	if *workArg == "" || *output == "" {
		return fmt.Errorf("-work-dir and -output are required")
	}
	if _, err := os.Stat(*output); !os.IsNotExist(err) {
		return fmt.Errorf("output must be a new file")
	}
	work := *workArg
	var state struct {
		Region, AssetBucket   string
		WindowsProbeInstances map[string]struct{ InstanceID, ImageID, ImageName string }
	}
	raw, err := os.ReadFile(filepath.Join(work, "resources.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	cli := &aws.CLI{Region: state.Region, SessionPath: filepath.Join(work, "harness-session.json")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	var results []map[string]any
	for _, year := range []string{"2022", "2025"} {
		item := state.WindowsProbeInstances[year]
		commands, err := aws.DiscoverGuestCommands(ctx, cli, item.InstanceID)
		if err != nil {
			return err
		}
		node := &aws.Node{API: cli, Input: &aws.S3Input{CLI: cli, Bucket: state.AssetBucket}, NodeName: year, InstanceID: item.InstanceID, Commands: commands}
		ps := func(script string) ([]byte, error) {
			return node.Exec(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
		}
		nics, err := ps(`(Get-NetAdapter -Physical | Measure-Object).Count`)
		if err != nil || strings.TrimSpace(string(nics)) != "1" {
			return fmt.Errorf("%s does not have one physical NIC: %s %v", year, nics, err)
		}
		marker, err := ps(`[Convert]::ToBase64String([IO.File]::ReadAllBytes('C:\ProgramData\CloudProvisioning\bootstrap.txt'))`)
		if err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(marker)))
		if err != nil {
			return err
		}
		if string(decoded) != `userdata & < > "quotes" Ω` {
			return fmt.Errorf("%s userdata changed: %q", year, decoded)
		}
		build, err := ps(`[Environment]::OSVersion.Version.Build`)
		if err != nil {
			return err
		}
		expected := map[string]string{"2022": "20348", "2025": "26100"}[year]
		if strings.TrimSpace(string(build)) != expected {
			return fmt.Errorf("unexpected Windows build %s", build)
		}
		destination := `C:\ProgramData\CloudProvisioning\binary & input.bin`
		if err := node.Put(ctx, bytes.NewReader(payload), destination, 0600); err != nil {
			return err
		}
		actual, err := ps(`[Convert]::ToBase64String([IO.File]::ReadAllBytes('C:\ProgramData\CloudProvisioning\binary & input.bin'))`)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(actual)) != base64.StdEncoding.EncodeToString(payload) {
			return fmt.Errorf("%s Put changed binary bytes", year)
		}
		actual, err = node.Pipe(ctx, bytes.NewReader(payload), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$m=New-Object IO.MemoryStream;[Console]::OpenStandardInput().CopyTo($m);[Console]::Write([Convert]::ToBase64String($m.ToArray()))`)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(actual)) != base64.StdEncoding.EncodeToString(payload) {
			return fmt.Errorf("%s Pipe changed binary bytes", year)
		}
		_, err = ps("exit 7")
		var exit *rig.ExitError
		if !errors.As(err, &exit) || exit.Code != 7 {
			return fmt.Errorf("%s lost native failure: %v", year, err)
		}
		acl, err := ps(`$a=Get-Acl -LiteralPath 'C:\ProgramData\CloudProvisioning\binary & input.bin'; if(-not $a.AreAccessRulesProtected){throw 'Inherited ACL'}; $a.Access.IdentityReference | ForEach-Object { $_.Translate([Security.Principal.SecurityIdentifier]).Value }`)
		if err != nil {
			return err
		}
		identities := strings.Fields(string(acl))
		if len(identities) != 2 || identities[0] == identities[1] {
			return fmt.Errorf("unexpected ACL %q", acl)
		}
		for _, id := range identities {
			if id != "S-1-5-18" && id != "S-1-5-32-544" {
				return fmt.Errorf("unexpected ACL principal %s", id)
			}
		}
		results = append(results, map[string]any{"windows": year, "build": expected, "image": item.ImageName, "physicalNICs": 1, "userdataPreserved": true, "binaryPutPreserved": true, "binaryStdinPreserved": true, "nativeFailurePreserved": true, "restrictedFileACL": true})
		fmt.Println("Windows", year, "native userdata and SSM transport passed")
	}
	report := map[string]any{"observedAt": time.Now().UTC(), "results": results, "scope": "native EC2Launch userdata and SSM transport only; no CAPI join, CNI or Windows GPU validation"}
	out, _ := json.MarshalIndent(report, "", "  ")
	return os.WriteFile(*output, append(out, '\n'), 0600)
}
