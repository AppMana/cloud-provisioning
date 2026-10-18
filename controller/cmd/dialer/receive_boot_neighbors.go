package main

import (
	"fmt"
	"os"
)

// Account for completed ordinary-neighbor bookkeeping without replaying it.
// Pending cleanup still requires live admission. Every admitted byte is retained
// by the boot caller's before/after checks under the shared native lock.
func readReceiveBootNeighborFiles(lockPath string, inventory receiveInventoryCheckpoint, withdrawn map[string]string) (map[string][]byte, error) {
	files := map[string][]byte{}
	expected := receiveNeighborCheckpoint{InventoryUID: inventory.InventoryUID, MeshUID: inventory.Inventory.MeshUID, NodeUID: inventory.NodeUID, PublicKey: inventory.PublicKey}
	path := lockPath + ".neighbors.json"
	for _, name := range []string{path, path + ".retiring"} {
		info, err := os.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("boot neighbor checkpoint must be a private regular file")
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		saved, err := decodeReceiveNeighborCheckpoint(raw, expected, name == path)
		if err != nil {
			return nil, err
		}
		if len(saved.Retiring) != 0 || len(saved.Candidates) != 0 {
			return nil, fmt.Errorf("boot neighbor checkpoint requires pending live reconciliation")
		}
		for _, peer := range saved.Peers {
			for _, public := range withdrawn {
				if peer.PublicKey == public {
					return nil, fmt.Errorf("boot neighbor checkpoint retains a withdrawn peer")
				}
			}
		}
		files[name] = raw
	}
	return files, nil
}
