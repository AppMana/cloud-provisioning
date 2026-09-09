package tunnelhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func deliveryFixture(t *testing.T) (*State, *observedDevice, DeliveryFiles, []byte) {
	t.Helper()
	s, d := fixture(t)
	s.RequestPath = filepath.Join(filepath.Dir(s.CachePath), "request.json")
	s.ReceiptPath = filepath.Join(filepath.Dir(s.CachePath), "receipt.json")
	f := DeliveryFiles{RequestPath: s.RequestPath, ReceiptPath: s.ReceiptPath, MaxReceiptAge: time.Minute}
	raw, err := json.MarshalIndent(tunnel.PeerListDoc{Peers: s.Identity.Peers}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return s, d, f, raw
}

func TestReceiptFollowsNativeApplicationAndExactSecretBytes(t *testing.T) {
	s, d, f, raw := deliveryFixture(t)
	if applied, err := f.Submit("uid", raw); err != nil || applied {
		t.Fatal("delivery alone was acknowledged")
	}
	d.err = errors.New("route install failed")
	if err := s.Start(); err == nil {
		t.Fatal("native failure lost")
	}
	if applied, err := f.Submit("uid", raw); err != nil || applied {
		t.Fatal("failed native apply was acknowledged")
	}
	d.err = nil
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.Submit("uid", raw); err != nil || !applied {
		t.Fatal("successful exact-byte delivery not acknowledged")
	}
	receiptRaw, err := os.ReadFile(s.ReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = json.Unmarshal(receiptRaw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Hash != tunnel.HashPeerList(raw) {
		t.Fatal("receipt hashed canonical JSON instead of Secret bytes")
	}
	// Native failure while reverting must not reuse an older successful receipt.
	removed := []byte(`{"peers":[]}`)
	if applied, err := f.Submit("uid", removed); err != nil || applied {
		t.Fatal("changed delivery inherited old receipt")
	}
	d.err = errors.New("partial native mutation")
	if err = s.Reconcile(); err == nil {
		t.Fatal("failure lost")
	}
	if _, err = os.Stat(s.ReceiptPath); !os.IsNotExist(err) {
		t.Fatal("receipt survived partial apply failure")
	}
	before := len(d.docs)
	if applied, err := f.Submit("uid", raw); err != nil || applied {
		t.Fatal("repeated older payload inherited old receipt")
	}
	d.err = nil
	if err = s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(d.docs) != before+1 {
		t.Fatal("reverting after partial failure skipped native apply")
	}
}

func TestReceiptExpiresAndCannotCrossSecretRecreation(t *testing.T) {
	s, _, f, raw := deliveryFixture(t)
	if _, err := f.Submit("old-uid", raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(s.ReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = json.Unmarshal(bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.AppliedAt = time.Now().Add(-2 * time.Minute)
	write(t, s.ReceiptPath, receipt)
	if applied, err := f.Submit("old-uid", raw); err != nil || applied {
		t.Fatal("stale receipt accepted")
	}
	if err = s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.Submit("new-uid", raw); err != nil || applied {
		t.Fatal("recreated Secret inherited receipt")
	}
	if err = s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.Submit("new-uid", raw); err != nil || !applied {
		t.Fatal("new delivery not acknowledged")
	}
	if err = s.ClearReceipt(); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.Submit("new-uid", raw); err != nil || applied {
		t.Fatal("stopped host still acknowledged")
	}
}

func TestInvalidDeliveryCannotMutateNativeState(t *testing.T) {
	s, d, f, _ := deliveryFixture(t)
	if _, err := f.Submit("uid", []byte(`{"peers":[],"privateKey":"injected"}`)); err == nil {
		t.Fatal("private key accepted in public request")
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// A structurally valid request still needs the host's full native validation.
	if _, err := f.Submit("uid", []byte(`{"peers":[{"publicKey":"bad"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err == nil {
		t.Fatal("invalid peer reached device")
	}
	if len(d.docs) != 1 {
		t.Fatal("invalid peer mutated device")
	}
	if applied, err := f.Submit("uid", []byte(`{"peers":[{"publicKey":"bad"}]}`)); err != nil || applied {
		t.Fatal("invalid peer acknowledged")
	}
}

type driftingDevice struct {
	observedDevice
	missing, cannotRepair bool
}

func (d *driftingDevice) Verify(_ tunnel.PeersFileDoc) error {
	if d.missing {
		return fmt.Errorf("kernel route missing")
	}
	return nil
}
func (d *driftingDevice) Apply(doc tunnel.PeersFileDoc, port uint16) error {
	if err := d.observedDevice.Apply(doc, port); err != nil {
		return err
	}
	if !d.cannotRepair {
		d.missing = false
	}
	return nil
}
func TestReceiptRequiresRepairOfUnchangedKernelRoutes(t *testing.T) {
	s, _ := fixture(t)
	d := &driftingDevice{}
	s.Device = d
	s.RequestPath = filepath.Join(t.TempDir(), "request.json")
	s.ReceiptPath = filepath.Join(t.TempDir(), "receipt.json")
	raw, err := json.Marshal(tunnel.PeerListDoc{Peers: s.Identity.Peers})
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.RequestPath, Request{ID: strings.Repeat("a", 32), SecretUID: "secret", Value: raw})
	if err = s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	calls := len(d.docs)
	if err = s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(d.docs) != calls {
		t.Fatal("healthy device was unnecessarily reapplied")
	}
	d.missing = true
	d.cannotRepair = true
	if err = s.Reconcile(); err == nil {
		t.Fatal("renewed receipt despite missing kernel route")
	}
	if _, err = os.Stat(s.ReceiptPath); !os.IsNotExist(err) {
		t.Fatal("stale receipt survived failed repair")
	}
	d.cannotRepair = false
	if err = s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if d.missing || len(d.docs) != calls+2 {
		t.Fatal("unchanged desired routes were not repaired")
	}
	if _, err = os.Stat(s.ReceiptPath); err != nil {
		t.Fatal("receipt absent after observed repair")
	}
}
