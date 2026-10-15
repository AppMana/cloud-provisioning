// receiveplan renders a public receive-only preparation plan. It applies nothing.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func render(input io.Reader, output io.Writer) error {
	var request struct {
		MeshID   string             `json:"meshID"`
		Previous tunnel.PeerListDoc `json:"previous"`
		Desired  tunnel.PeerListDoc `json:"desired"`
		Native   []netip.Prefix     `json:"native"`
	}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("exactly one public plan request required")
	}
	plan, err := tunnel.PlanReceiveHandover(request.MeshID, request.Previous, request.Desired, request.Native)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(plan)
}

func main() {
	if err := render(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
