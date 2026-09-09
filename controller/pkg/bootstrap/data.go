// Package bootstrap defines the guest bootstrap contract independently of
// infrastructure transport and Kubernetes distribution.
package bootstrap

import (
	"bytes"
	"encoding/base64"
	"fmt"
)

const (
	CloudConfig = "cloud-config"
	Ignition    = "ignition"
	PowerShell  = "powershell"
)

// Data is the value and format stored in a CAPI bootstrap Secret. Value is
// never base64 encoded here; infrastructure APIs own their transport encoding.
type Data struct {
	Format string
	Value  []byte
}

func (d Data) Validate() error {
	if len(bytes.TrimSpace(d.Value)) == 0 {
		return fmt.Errorf("empty bootstrap data")
	}
	switch d.Format {
	case CloudConfig, Ignition, PowerShell:
		return nil
	default:
		return fmt.Errorf("unsupported bootstrap format %q", d.Format)
	}
}

// EC2Launch renders the uncompressed, once-only EC2Launch Windows envelope.
// Callers must set AWSMachine.spec.uncompressedUserData. CAPA's Linux secure
// bootstrap wrapper is not a consumer of this format.
func (d Data) EC2Launch() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d.Format != PowerShell {
		return nil, fmt.Errorf("EC2Launch requires powershell, got %q", d.Format)
	}
	// EC2Launch v2 extracts the tag body without XML entity decoding. XML
	// escaping turns quotes/newlines into literal &#39;/&#xA; PowerShell syntax
	// errors (observed on Server 2022 and 2025). An ASCII wrapper also avoids
	// Windows PowerShell 5 interpreting a UTF-8 script without a BOM as ANSI.
	// This inner payload encoding is independent of EC2 API transport encoding.
	payload := base64.StdEncoding.EncodeToString(d.Value)
	return []byte("<powershell>\n$ErrorActionPreference = 'Stop'\n" +
		"Invoke-Expression ([System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('" + payload + "')))\n" +
		"</powershell>\n<persist>false</persist>\n"), nil
}
