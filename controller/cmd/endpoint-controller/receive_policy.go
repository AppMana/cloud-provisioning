package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// receivePreparationPolicy is operator-supplied network policy, not inferred
// permission to open a firewall. It enables additive preparation only.
type receivePreparationPolicy struct {
	NativeCIDRs []netip.Prefix        `json:"nativeCIDRs"`
	Ports       []int                 `json:"ports"`
	Journal     *receiveJournalPolicy `json:"journal,omitempty"`
}

type receiveJournalPolicy struct {
	RoundLifetime metav1.Duration       `json:"roundLifetime"`
	MaxReceiptAge metav1.Duration       `json:"maxReceiptAge"`
	DrainInterval metav1.Duration       `json:"drainInterval"`
	Receipts      *receiveReceiptPolicy `json:"receipts,omitempty"`
}

func (p receiveJournalPolicy) policy() (handover.Policy, error) {
	v := handover.Policy{RoundLifetime: p.RoundLifetime.Duration, MaxReceiptAge: p.MaxReceiptAge.Duration, DrainInterval: p.DrainInterval.Duration}
	if v.RoundLifetime <= 0 || v.MaxReceiptAge <= 0 || v.MaxReceiptAge > v.RoundLifetime || v.DrainInterval <= 0 {
		return handover.Policy{}, fmt.Errorf("journal requires positive explicit round, receipt and drain durations; receipt age must not exceed round lifetime")
	}
	return v, nil
}

func parseReceivePreparationPolicy(raw, legacyPort string) (*receivePreparationPolicy, error) {
	if raw == "" {
		return nil, nil
	}
	p := &receivePreparationPolicy{}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(p); err != nil {
		return nil, fmt.Errorf("invalid receiver preparation policy: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing receiver preparation policy input")
	}
	if len(p.NativeCIDRs) == 0 || len(p.Ports) == 0 {
		return nil, fmt.Errorf("receiver preparation requires native CIDRs and explicitly permitted ports")
	}
	if p.Journal != nil {
		if _, err := p.Journal.policy(); err != nil {
			return nil, err
		}
		if p.Journal.Receipts != nil {
			if err := p.Journal.Receipts.validate(); err != nil {
				return nil, err
			}
		}
	}
	seen := map[netip.Prefix]bool{}
	for _, prefix := range p.NativeCIDRs {
		if !prefix.IsValid() || prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || seen[prefix] {
			return nil, fmt.Errorf("invalid or duplicate receiver native CIDR")
		}
		seen[prefix] = true
	}
	legacy, err := strconv.Atoi(legacyPort)
	if err != nil || legacy < 1 || legacy > 65535 {
		return nil, fmt.Errorf("receiver preparation requires a valid legacy listener port")
	}
	ports := map[int]bool{}
	for _, port := range p.Ports {
		if port < 1 || port > 65535 || port == legacy || ports[port] {
			return nil, fmt.Errorf("invalid, duplicate or legacy receiver port")
		}
		ports[port] = true
	}
	return p, nil
}
