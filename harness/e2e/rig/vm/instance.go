package vm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type launchReceipt struct {
	UID       string `json:"uid"`
	Hash      string `json:"hash"`
	WrapperID string `json:"wrapperID"`
	StartedAt string `json:"startedAt,omitempty"`
	Phase     string `json:"phase"`
}

type wrapperObservation struct {
	ID    string `json:"Id"`
	State struct {
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
}

func (n *Node) observeWrapper(ctx context.Context) (wrapperObservation, error) {
	var state wrapperObservation
	out, _, code, err := n.run(ctx, nil, "docker", "inspect", "--format", `{"Id":{{json .Id}},"State":{{json .State}}}`, n.Wrapper())
	if err != nil || code != 0 {
		return state, fmt.Errorf("observing wrapper %s failed: %v (exit %d)", n.Name(), err, code)
	}
	if err := json.Unmarshal(out, &state); err != nil || state.ID == "" || state.State.StartedAt == "" {
		return state, fmt.Errorf("wrapper %s has no complete identity", n.Name())
	}
	return state, nil
}

// BootstrapInstance records launch completion before waiting for the guest.
// A timeout while observing a boot never authorizes resetting its disk again.
func (n *Node) BootstrapInstance(ctx context.Context, uid string, data rig.BootstrapData) error {
	if n.rig == nil || uid == "" || data.Format != rig.CloudConfig {
		return fmt.Errorf("VM rig, infrastructure UID and cloud-config required")
	}
	if err := data.Validate(); err != nil {
		return err
	}
	dir := n.rig.SeedDir(n.Name())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "launch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("VM launch observation already in progress: %w", err)
	}
	path := filepath.Join(dir, "launch.json")
	hash := fmt.Sprintf("%x", sha256.Sum256(data.Value))
	state, err := n.observeWrapper(ctx)
	if err != nil {
		return err
	}
	var previous launchReceipt
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &previous); err != nil || previous.UID == "" || previous.Hash == "" || previous.WrapperID == "" {
			return fmt.Errorf("invalid VM launch receipt")
		}
		if previous.WrapperID != state.ID {
			return fmt.Errorf("VM wrapper identity changed")
		}
		if previous.UID == uid {
			if previous.Hash != hash {
				return fmt.Errorf("bootstrap data changed for the same infrastructure UID")
			}
			if previous.Phase != "launched" {
				return fmt.Errorf("VM launch outcome unresolved; inspect the existing instance before recovery")
			}
			if !state.State.Running || previous.StartedAt != state.State.StartedAt {
				return fmt.Errorf("launched VM stopped or its boot identity changed")
			}
			return n.rig.waitForNode(ctx, n.Name(), BootTimeout)
		}
		if state.State.Running {
			return fmt.Errorf("previous infrastructure instance still running in this slot")
		}
	}
	receipt := launchReceipt{UID: uid, Hash: hash, WrapperID: state.ID, Phase: "launching"}
	if err := writeLaunchReceipt(path, receipt); err != nil {
		return err
	}
	if err := n.launchUserdata(ctx, data.Value); err != nil {
		return err
	}
	started, err := n.observeWrapper(ctx)
	if err != nil {
		return err
	}
	if started.ID != state.ID || !started.State.Running || started.State.StartedAt == state.State.StartedAt {
		return fmt.Errorf("new VM boot identity was not observed")
	}
	receipt.Phase, receipt.StartedAt = "launched", started.State.StartedAt
	if err := writeLaunchReceipt(path, receipt); err != nil {
		return err
	}
	return n.rig.waitForNode(ctx, n.Name(), BootTimeout)
}

func writeLaunchReceipt(path string, receipt launchReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".launch-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
