// gatewayplan renders a proposed native-Ethernet attachment without applying
// cloud or Kubernetes mutations. Input is a public, observed topology snapshot.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	var input struct {
		Request     attachment.GatewayRequest `json:"request"`
		GatewayKey  string                    `json:"gatewayKey"`
		SitePeers   []tunnel.PeerSpec         `json:"sitePeers"`
		WorkerPeers []tunnel.PeerSpec         `json:"workerPeers"`
	}
	decoder := json.NewDecoder(io.LimitReader(in, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return fmt.Errorf("read topology: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one topology document")
	}
	plan, err := attachment.PlanGateway(input.Request)
	if err != nil {
		return err
	}
	site, err := attachment.GatewayPeers(plan, input.SitePeers, input.GatewayKey, false)
	if err != nil {
		return err
	}
	worker, err := attachment.GatewayPeers(plan, input.WorkerPeers, input.GatewayKey, true)
	if err != nil {
		return err
	}
	result := struct {
		Scope       string                 `json:"scope"`
		Plan        attachment.GatewayPlan `json:"plan"`
		SitePeers   []tunnel.PeerSpec      `json:"sitePeers"`
		WorkerPeers []tunnel.PeerSpec      `json:"workerPeers"`
	}{"Proposed attachment only; no forwarding applied or readiness established", plan, site, worker}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
