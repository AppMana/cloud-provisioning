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
	// Transport fields bind an individual path when a node has several
	// devices, or keeps its local key while changing the remote peer key.
	DeviceID    string `json:"deviceID,omitempty"`
	FromPeerKey string `json:"fromPeerKey,omitempty"`
	ToPeerKey   string `json:"toPeerKey,omitempty"`
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
	// These fields are stamped by authentication from live API objects, not
	// trusted from submissions. Persisting them permits dependency consumers
	// to reject observations from a replaced pod or an earlier node boot.
	ObservedBootID     string `json:"observedBootID,omitempty"`
	AgentPodName       string `json:"agentPodName,omitempty"`
	AgentPodUID        string `json:"agentPodUID,omitempty"`
	AgentContainerName string `json:"agentContainerName,omitempty"`
	// AgentContainerID identifies the process incarnation that made the native
	// observation. Pod-bound credentials alone survive container restarts.
	AgentContainerID string    `json:"agentContainerID,omitempty"`
	RoundHash        string    `json:"roundHash"`
	NodeUID          string    `json:"nodeUID"`
	FromKey          string    `json:"fromKey"`
	ToKey            string    `json:"toKey"`
	ObservedAt       time.Time `json:"observedAt"`
	DeviceID         string    `json:"deviceID,omitempty"`
	FromPeerKey      string    `json:"fromPeerKey,omitempty"`
	ToPeerKey        string    `json:"toPeerKey,omitempty"`
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
	transports := r.Members[0].DeviceID != ""
	members := map[[2]string]bool{}
	legacyKeys := map[wgtypes.Key]bool{}
	owners := map[wgtypes.Key]string{}
	pairs := map[[2]wgtypes.Key]bool{}
	for i, m := range r.Members {
		id := [2]string{m.NodeUID, m.DeviceID}
		if !token(m.NodeUID) || members[id] || (m.DeviceID != "") != transports {
			return Round{}, fmt.Errorf("invalid or duplicate handover member")
		}
		members[id] = true
		if transports {
			if !token(m.DeviceID) {
				return Round{}, fmt.Errorf("invalid transport identity")
			}
		} else if m.FromPeerKey != "" || m.ToPeerKey != "" {
			return Round{}, fmt.Errorf("peer identities require a transport identity")
		}
		local := [2]*string{&m.FromKey, &m.ToKey}
		remote := [2]*string{&m.FromPeerKey, &m.ToPeerKey}
		for generation, value := range local {
			key, e := wgtypes.ParseKey(*value)
			if e != nil || key == (wgtypes.Key{}) {
				return Round{}, fmt.Errorf("invalid generation device key")
			}
			*value = key.String()
			if !transports {
				if legacyKeys[key] {
					return Round{}, fmt.Errorf("reused generation device key")
				}
				legacyKeys[key] = true
				continue
			}
			if owner, found := owners[key]; found && owner != m.NodeUID {
				return Round{}, fmt.Errorf("device key belongs to another node")
			}
			owners[key] = m.NodeUID
			peer, e := wgtypes.ParseKey(*remote[generation])
			if e != nil || peer == (wgtypes.Key{}) || peer == key {
				return Round{}, fmt.Errorf("invalid transport peer key")
			}
			*remote[generation] = peer.String()
			pair := [2]wgtypes.Key{key, peer}
			if pairs[pair] {
				return Round{}, fmt.Errorf("reused transport key pair")
			}
			pairs[pair] = true
		}
		r.Members[i] = m
	}
	sort.Slice(r.Members, func(i, j int) bool {
		if r.Members[i].NodeUID != r.Members[j].NodeUID {
			return r.Members[i].NodeUID < r.Members[j].NodeUID
		}
		return r.Members[i].DeviceID < r.Members[j].DeviceID
	})
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
	return r.validateReceipts(now, receipts, true)
}

// ValidateReceipts validates a nonempty subset of current round observations.
// It does not authenticate them, complete the round, or authorize retirement.
func (r Round) ValidateReceipts(now time.Time, receipts []AuthenticatedReceipt) error {
	return r.validateReceipts(now, receipts, false)
}

func (r Round) validateReceipts(now time.Time, receipts []AuthenticatedReceipt, complete bool) error {
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
	if len(receipts) == 0 || len(receipts) > len(r.Members) || (complete && len(receipts) != len(r.Members)) {
		return fmt.Errorf("incomplete handover receipts")
	}
	members := map[[2]string]Member{}
	for _, m := range r.Members {
		members[[2]string{m.NodeUID, m.DeviceID}] = m
	}
	seen := map[[2]string]bool{}
	for _, authenticated := range receipts {
		receipt := authenticated.Receipt
		id := [2]string{receipt.NodeUID, receipt.DeviceID}
		member, ok := members[id]
		if !ok || seen[id] || authenticated.AuthenticatedNodeUID != receipt.NodeUID {
			return fmt.Errorf("receipt node identity mismatch or duplicate")
		}
		seen[id] = true
		if receipt.RoundHash != digest || receipt.FromKey != member.FromKey || receipt.ToKey != member.ToKey || receipt.FromPeerKey != member.FromPeerKey || receipt.ToPeerKey != member.ToPeerKey {
			return fmt.Errorf("receipt does not bind the current round and devices")
		}
		if receipt.ObservedAt.Before(r.IssuedAt) || receipt.ObservedAt.After(now) || now.Sub(receipt.ObservedAt) > r.MaxReceiptAge {
			return fmt.Errorf("receipt observation is stale or in the future")
		}
	}
	return nil
}
