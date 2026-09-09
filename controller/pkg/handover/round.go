// Package handover contains shared protocol primitives for generation handover.
// It is not wired into the production publisher or native backends yet.
package handover

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type Phase string

const (
	Prepare Phase = "prepare"
	Switch  Phase = "switch"
	Drain   Phase = "drain"
	Retire  Phase = "retire"
	Abort   Phase = "abort"
)

// Member binds immutable Kubernetes identity to both devices. Names and IPs
// cannot stand in for a Node UID: replacement may reuse either of them.
type Member struct {
	NodeUID string `json:"nodeUID"`
	FromKey string `json:"fromKey"`
	ToKey   string `json:"toKey"`
}

// Spec refers to the hash of the complete immutable, validated public graph.
// This package checks that hash's representation, not the graph itself.
type Spec struct {
	ClusterUID     string   `json:"clusterUID"`
	TransitionUID  string   `json:"transitionUID"`
	PlanHash       string   `json:"planHash"`
	FromGeneration string   `json:"fromGeneration"`
	ToGeneration   string   `json:"toGeneration"`
	Phase          Phase    `json:"phase"`
	Members        []Member `json:"members"`
}

// Round must be persisted with a storage compare-and-swap before publication.
// Every phase attempt, including rollback to a previously selected generation,
// needs a fresh round. Reconstruct persisted rounds on restart; do not mint a
// replacement nonce merely because an observation timed out.
type Round struct {
	Spec
	Nonce         string        `json:"nonce"`
	IssuedAt      time.Time     `json:"issuedAt"`
	ExpiresAt     time.Time     `json:"expiresAt"`
	MaxReceiptAge time.Duration `json:"maxReceiptAge"`
}

type Receipt struct {
	RoundHash  string    `json:"roundHash"`
	NodeUID    string    `json:"nodeUID"`
	FromKey    string    `json:"fromKey"`
	ToKey      string    `json:"toKey"`
	ObservedAt time.Time `json:"observedAt"`
}

// AuthenticatedReceipt is constructed by the transport's authentication layer.
// AuthenticatedNodeUID must come from that layer, never from receipt payloads.
// Hosts may issue receipts only after the phase's native verification succeeds.
type AuthenticatedReceipt struct {
	AuthenticatedNodeUID string `json:"-"`
	Receipt              Receipt
}

func NewRound(spec Spec, now time.Time, lifetime, maxReceiptAge time.Duration) (Round, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Round{}, err
	}
	r := Round{Spec: spec, Nonce: hex.EncodeToString(nonce[:]), IssuedAt: now.UTC(), ExpiresAt: now.Add(lifetime).UTC(), MaxReceiptAge: maxReceiptAge}
	return r.canonical()
}

// DecodeRound restores exactly one known-schema round. Unknown fields must not
// disappear silently when a newer persisted protocol is read by an older host.
func DecodeRound(raw []byte) (Round, error) {
	var r Round
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return Round{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Round{}, fmt.Errorf("trailing handover state")
	}
	return r.canonical()
}

func token(s string) bool { return s != "" && strings.TrimSpace(s) == s }
func hexToken(s string, n int) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == n && hex.EncodeToString(b) == s
}

func (r Round) canonical() (Round, error) {
	if !token(r.ClusterUID) || !token(r.TransitionUID) || !token(r.FromGeneration) || !token(r.ToGeneration) || r.FromGeneration == r.ToGeneration || !hexToken(r.PlanHash, 32) || !hexToken(r.Nonce, 16) {
		return Round{}, fmt.Errorf("invalid handover round identity")
	}
	switch r.Phase {
	case Prepare, Switch, Drain, Retire, Abort:
	default:
		return Round{}, fmt.Errorf("invalid handover phase")
	}
	if r.IssuedAt.IsZero() || !r.ExpiresAt.After(r.IssuedAt) || r.MaxReceiptAge <= 0 || r.MaxReceiptAge > r.ExpiresAt.Sub(r.IssuedAt) {
		return Round{}, fmt.Errorf("invalid handover round lifetime")
	}
	if len(r.Members) == 0 {
		return Round{}, fmt.Errorf("empty handover membership")
	}
	r.Members = append([]Member(nil), r.Members...)
	nodes := map[string]bool{}
	keys := map[wgtypes.Key]bool{}
	for i, m := range r.Members {
		if !token(m.NodeUID) || nodes[m.NodeUID] {
			return Round{}, fmt.Errorf("invalid or duplicate node UID")
		}
		nodes[m.NodeUID] = true
		values := []*string{&m.FromKey, &m.ToKey}
		for _, value := range values {
			key, e := wgtypes.ParseKey(*value)
			if e != nil || key == (wgtypes.Key{}) || keys[key] {
				return Round{}, fmt.Errorf("invalid or reused generation device key")
			}
			keys[key] = true
			*value = key.String()
		}
		r.Members[i] = m
	}
	sort.Slice(r.Members, func(i, j int) bool { return r.Members[i].NodeUID < r.Members[j].NodeUID })
	r.IssuedAt = r.IssuedAt.UTC()
	r.ExpiresAt = r.ExpiresAt.UTC()
	return r, nil
}

func (r Round) Digest() (string, error) {
	normalized, e := r.canonical()
	if e != nil {
		return "", e
	}
	// Domain separation prevents a hash for another public document type from
	// being mistaken for a phase-round binding.
	raw, e := json.Marshal(struct {
		Kind  string `json:"kind"`
		Round Round  `json:"round"`
	}{"cldt/handover-round/v1", normalized})
	if e != nil {
		return "", e
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

// Ready validates a complete set of current, identity-bound receipts. It does
// not authenticate the transport, verify native state, advance phases, or
// authorize retirement without durable phase history and a drain policy.
// Missing, duplicate, unknown or invalid receipts fail the entire round.
func (r Round) Ready(now time.Time, receipts []AuthenticatedReceipt) error {
	normalized, e := r.canonical()
	if e != nil {
		return e
	}
	r = normalized
	if now.Before(r.IssuedAt) || !now.Before(r.ExpiresAt) {
		return fmt.Errorf("handover round is not current")
	}
	digest, e := r.Digest()
	if e != nil {
		return e
	}
	if len(receipts) != len(r.Members) {
		return fmt.Errorf("incomplete handover receipts")
	}
	members := map[string]Member{}
	for _, m := range r.Members {
		members[m.NodeUID] = m
	}
	seen := map[string]bool{}
	for _, authenticated := range receipts {
		receipt := authenticated.Receipt
		member, ok := members[receipt.NodeUID]
		if !ok || seen[receipt.NodeUID] || authenticated.AuthenticatedNodeUID != receipt.NodeUID {
			return fmt.Errorf("receipt node identity mismatch or duplicate")
		}
		seen[receipt.NodeUID] = true
		if receipt.RoundHash != digest || receipt.FromKey != member.FromKey || receipt.ToKey != member.ToKey {
			return fmt.Errorf("receipt does not bind the current round and devices")
		}
		if receipt.ObservedAt.Before(r.IssuedAt) || receipt.ObservedAt.After(now) || now.Sub(receipt.ObservedAt) > r.MaxReceiptAge {
			return fmt.Errorf("receipt observation is stale or in the future")
		}
	}
	return nil
}
