// Package tunnelhost reconciles a host-owned device with immutable bootstrap
// identity and durable public peer updates delivered after cluster joining.
package tunnelhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice"
)

// State must be used by one service owner. Paths must be in a directory writable
// only by SYSTEM/Administrators on Windows. Updates contain no private identity.
type State struct {
	Device      tunneldevice.Device
	Identity    tunnel.PeersFileDoc
	Port        uint16
	UpdatesPath string
	CachePath   string
	RequestPath string
	ReceiptPath string
	applied     []byte
}

func decode(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON document")
	}
	return nil
}

func ReadIdentity(path string) (tunnel.PeersFileDoc, error) {
	var doc tunnel.PeersFileDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	if err = decode(raw, &doc); err != nil {
		return doc, err
	}
	_, err = tunneldevice.Compile(doc)
	return doc, err
}

func readPublic(path string) (tunnel.PeerListDoc, error) {
	var doc tunnel.PeerListDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	return publicBytes(raw)
}

func (s *State) native(public tunnel.PeerListDoc) (tunnel.PeersFileDoc, error) {
	if err := validateBackends(public.APIServers); err != nil {
		return tunnel.PeersFileDoc{}, err
	}
	doc := tunnel.PeersFileDoc{PrivateKey: s.Identity.PrivateKey, LocalAddress: s.Identity.LocalAddress, Peers: public.Peers, APIServers: public.APIServers}
	_, err := tunneldevice.Compile(doc)
	return doc, err
}

// Start restores the latest durable desired state, even if the update delivery
// pod cannot start yet. Corrupt existing state fails startup instead of silently
// resurrecting bootstrap peers. Missing files are the only bootstrap fallback.
func (s *State) Start() error {
	s.applied = nil
	if (s.RequestPath == "") != (s.ReceiptPath == "") {
		return fmt.Errorf("request and receipt paths must be configured together")
	}
	if err := s.ClearReceipt(); err != nil {
		return err
	}
	if s.Port == 0 || s.CachePath == "" || s.UpdatesPath == "" || filepath.Clean(s.CachePath) == filepath.Clean(s.UpdatesPath) {
		return fmt.Errorf("port and distinct update/cache paths are required")
	}
	if handled, err := s.requested(); handled {
		return err
	}
	public, err := readPublic(s.UpdatesPath)
	if errors.Is(err, os.ErrNotExist) {
		public, err = readPublic(s.CachePath)
	}
	if errors.Is(err, os.ErrNotExist) {
		public = tunnel.PeerListDoc{Peers: s.Identity.Peers, APIServers: s.Identity.APIServers}
		if public.Peers == nil {
			public.Peers = []tunnel.PeerSpec{}
		}
		err = nil
	}
	if err != nil {
		return fmt.Errorf("reading initial public state: %w", err)
	}
	return s.apply(public)
}

// Reconcile leaves the device alone on absent/malformed updates. Valid desired
// state is durable before applying it; failed native operations retry next poll.
func (s *State) Reconcile() error {
	if handled, err := s.requested(); handled {
		return err
	}
	public, err := readPublic(s.UpdatesPath)
	if errors.Is(err, os.ErrNotExist) {
		public, err = readPublic(s.CachePath)
	}
	if err != nil {
		return fmt.Errorf("reading public state: %w", err)
	}
	return s.apply(public)
}

func (s *State) apply(public tunnel.PeerListDoc) error {
	doc, err := s.native(public)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(public)
	if err != nil {
		return err
	}
	if err = save(s.CachePath, raw); err != nil {
		return fmt.Errorf("persisting public state: %w", err)
	}
	if bytes.Equal(s.applied, raw) {
		if observer, ok := s.Device.(tunneldevice.RouteObserver); ok {
			if err := observer.Verify(doc); err == nil {
				return nil
			}
			// A previously applied document is not evidence of current routes.
		} else {
			return nil
		}
	}
	s.applied = nil // A failed native apply may already have changed kernel state.
	if err := s.Device.Apply(doc, s.Port); err != nil {
		return err
	}
	if observer, ok := s.Device.(tunneldevice.RouteObserver); ok {
		if err := observer.Verify(doc); err != nil {
			return err
		}
	}
	s.applied = raw
	return nil
}

func save(path string, raw []byte) error {
	old, err := os.ReadFile(path)
	if err == nil && bytes.Equal(old, raw) {
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".peers-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
