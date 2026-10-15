package main

import (
	"fmt"
	"net"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Validate the entire input before changing policy rules or any route.
func validateSiteTransit(transit *tunnel.TransitSpec) error {
	if transit == nil {
		return nil
	}
	if net.ParseIP(transit.Via) == nil || (transit.ViaOtherFamily != "" && net.ParseIP(transit.ViaOtherFamily) == nil) {
		return fmt.Errorf("invalid transit gateway")
	}
	for _, h := range transit.Hosts {
		if _, err := parseHostRoute(h); err != nil {
			return err
		}
	}
	for _, b := range transit.Blocks {
		if _, err := blockRoute(b, 0); err != nil {
			return err
		}
	}
	return nil
}
