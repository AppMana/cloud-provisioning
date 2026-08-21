module github.com/appmana/cloud-provisioning/harness/e2e

go 1.26.0

replace github.com/appmana/cloud-provisioning/controller => ../../controller

require (
	github.com/appmana/cloud-provisioning/controller v0.0.0-00010101000000-000000000000
	sigs.k8s.io/yaml v1.6.0
)

require go.yaml.in/yaml/v2 v2.4.3 // indirect
