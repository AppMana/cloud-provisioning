package tunnelhost

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Request keeps the exact Secret bytes (JSON encodes Value as base64). ID is a
// fresh delivery nonce, including when an older payload is requested again.
// Public peer requests never contain the bootstrap private key.
type Request struct {
	ID        string `json:"id"`
	SecretUID string `json:"secretUID"`
	Value     []byte `json:"value"`
}

type Receipt struct {
	ID        string    `json:"id"`
	SecretUID string    `json:"secretUID"`
	Hash      string    `json:"hash"`
	AppliedAt time.Time `json:"appliedAt"`
}

func publicBytes(raw []byte) (tunnel.PeerListDoc, error) {
	var doc tunnel.PeerListDoc
	if err := decode(raw, &doc); err != nil {
		return doc, err
	}
	if doc.Peers == nil {
		return doc, fmt.Errorf("public update must contain a peers array")
	}
	if err := validateBackends(doc.APIServers); err != nil {
		return doc, err
	}
	return doc, nil
}

func readRequest(path string) (Request, error) {
	var req Request
	raw, err := os.ReadFile(path)
	if err != nil {
		return req, err
	}
	if err = decode(raw, &req); err != nil {
		return req, err
	}
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(req.ID) || req.SecretUID == "" {
		return req, fmt.Errorf("invalid delivery identity")
	}
	if _, err = publicBytes(req.Value); err != nil {
		return req, err
	}
	return req, nil
}

// DeliveryFiles is the publisher's side of the host protocol. The publisher can
// write requests and read receipts; only the host owner writes receipts.
type DeliveryFiles struct {
	RequestPath   string
	ReceiptPath   string
	MaxReceiptAge time.Duration
}

func (f DeliveryFiles) Submit(uid string, raw []byte) (bool, error) {
	if uid == "" || f.RequestPath == "" || f.ReceiptPath == "" || f.RequestPath == f.ReceiptPath || f.MaxReceiptAge <= 0 {
		return false, fmt.Errorf("invalid delivery configuration")
	}
	if _, err := publicBytes(raw); err != nil {
		return false, err
	}
	req, err := readRequest(f.RequestPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err != nil || req.SecretUID != uid || !bytes.Equal(req.Value, raw) {
		nonce := make([]byte, 16)
		if _, err = rand.Read(nonce); err != nil {
			return false, err
		}
		req = Request{ID: hex.EncodeToString(nonce), SecretUID: uid, Value: raw}
		encoded, err := json.Marshal(req)
		if err != nil {
			return false, err
		}
		if err = save(f.RequestPath, encoded); err != nil {
			return false, err
		}
		return false, nil
	}
	receiptRaw, err := os.ReadFile(f.ReceiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var receipt Receipt
	if err = decode(receiptRaw, &receipt); err != nil {
		return false, err
	}
	age := time.Since(receipt.AppliedAt)
	return receipt.ID == req.ID && receipt.SecretUID == uid && receipt.Hash == tunnel.HashPeerList(raw) && age >= 0 && age <= f.MaxReceiptAge, nil
}

func (s *State) ClearReceipt() error {
	if s.ReceiptPath == "" {
		return nil
	}
	err := os.Remove(s.ReceiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *State) requested() (bool, error) {
	if s.RequestPath == "" {
		return false, nil
	}
	req, err := readRequest(s.RequestPath)
	// Withdraw a receipt before any operation that could partially change the
	// device. A durable desired document alone is never an applied acknowledgment.
	if e := s.ClearReceipt(); e != nil {
		return true, e
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	public, err := publicBytes(req.Value)
	if err != nil {
		return true, err
	}
	if err = s.apply(public); err != nil {
		return true, err
	}
	receipt := Receipt{ID: req.ID, SecretUID: req.SecretUID, Hash: tunnel.HashPeerList(req.Value), AppliedAt: time.Now().UTC()}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return true, err
	}
	return true, save(s.ReceiptPath, raw)
}
