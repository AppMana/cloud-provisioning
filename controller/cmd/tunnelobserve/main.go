//go:build linux || windows

// tunnelobserve samples an existing adapter without changing it.
// Output contains hashed public identities and counters, never configuration keys.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"
)

type peerObservation struct {
	Identity  string `json:"publicKeySHA256"`
	Endpoint  string `json:"endpoint"`
	Tx        uint64 `json:"txBytes"`
	Rx        uint64 `json:"rxBytes"`
	Handshake uint64 `json:"lastHandshakeFiletime"`
}

type adapterReader struct {
	read  func() (bool, []peerObservation, error)
	close func()
}

func run() error {
	name := flag.String("interface", "", "existing cldt adapter name")
	report := flag.String("report", "", "new JSONL observation file")
	samples := flag.Int("samples", 1, "number of samples, at most 3600")
	interval := flag.Duration("interval", time.Second, "sampling interval, 100ms to 1m")
	flag.Parse()
	if !regexp.MustCompile(`^cldt[0-9a-f]{8}$`).MatchString(*name) || *report == "" ||
		*samples < 1 || *samples > 3600 || *interval < 100*time.Millisecond || *interval > time.Minute || time.Duration(*samples-1)*(*interval) > time.Hour {
		return fmt.Errorf("require an existing cldt interface, new report, and bounded sampling")
	}
	adapter, err := openAdapter(*name)
	if err != nil {
		return err
	}
	defer adapter.close()
	f, err := os.OpenFile(*report, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	for index := 0; index < *samples; index++ {
		up, peers, err := adapter.read()
		if err != nil {
			return err
		}

		sort.Slice(peers, func(i, j int) bool { return peers[i].Identity < peers[j].Identity })
		// Only serialize the explicitly selected observation fields.
		if err = encoder.Encode(struct {
			ObservedAt time.Time         `json:"observedAt"`
			Interface  string            `json:"interface"`
			Up         bool              `json:"up"`
			Peers      []peerObservation `json:"peers"`
		}{time.Now().UTC(), *name, up, peers}); err != nil {
			return err
		}
		if err = f.Sync(); err != nil {
			return err
		}
		if index+1 < *samples {
			time.Sleep(*interval)
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"samples": *samples, "report": *report})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
