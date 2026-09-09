// This nested module excludes the release-specific helper from harness builds.
// build.sh compiles main.go using the exact MCO module and its vendor tree.
module github.com/appmana/cloud-provisioning/okd-exporter

go 1.24.8
