// Package bootstrap defines the guest bootstrap contract independently of
// infrastructure transport and Kubernetes distribution.
package bootstrap

import labbootstrap "github.com/appmana/labcontainers/pkg/bootstrap"

const (
	CloudConfig = labbootstrap.CloudConfig
	Ignition    = labbootstrap.Ignition
	PowerShell  = labbootstrap.PowerShell
)

// Data is the value and format stored in a CAPI bootstrap Secret. Value is
// never base64 encoded here; infrastructure APIs own their transport encoding.
type Data = labbootstrap.Data
