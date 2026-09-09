package main

import "github.com/appmana/cloud-provisioning/controller/pkg/tunnel"

func canonicalAPIHosts(values ...string) []string { return tunnel.CanonicalAPIHosts(values...) }
