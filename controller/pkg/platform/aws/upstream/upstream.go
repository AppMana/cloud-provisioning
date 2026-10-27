// Package upstream holds the AWS VPC CNI as its pinned upstream chart
// renders it, for the controller to adapt to each cluster.
package upstream

import _ "embed"

//go:generate ./render.sh

// Manifest is aws-vpc-cni chart 1.23.2 (sha256 4572602…618f) rendered with
// values.yaml.
//
//go:embed aws-vpc-cni.yaml
var Manifest []byte
