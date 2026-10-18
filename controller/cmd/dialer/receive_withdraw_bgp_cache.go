package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type withdrawnAdvertisementCache struct {
	path string
	raw  []byte
	next transitAdvertisementCache
}

// Keep the existing cache format. The new BGP hash is written before forwarding
// removal. An interruption leaves a non-replayable pair; a live admitted retry
// accepts only the current or exact withdrawal projection, never arbitrary skew.
func prepareWithdrawnAdvertisementCache(cfg config, speaker *transitSpeaker, forwarding *withdrawnForwardingCache) (*withdrawnAdvertisementCache, error) {
	path := transitAdvertisementCachePath(cfg)
	if path == "" {
		return nil, nil
	}
	raw, err := readPrivateRetirementCache(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if speaker == nil || forwarding == nil {
		return nil, fmt.Errorf("advertisement withdrawal requires matching forwarding and speaker")
	}
	var cached transitAdvertisementCache
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cached); err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("trailing advertisement withdrawal cache")
	}
	var previous siteForwardingCache
	if err := json.Unmarshal(forwarding.raw, &previous); err != nil {
		return nil, err
	}
	// A prior interrupted commit may already contain the next hash. Validate all
	// speaker, peer and identity fields against that exact projection as well.
	oldErr := validateTransitAdvertisementCache(cached, cfg, speaker, &previous)
	nextErr := validateTransitAdvertisementCache(cached, cfg, speaker, &forwarding.next)
	if oldErr != nil && nextErr != nil {
		return nil, fmt.Errorf("advertisement cache differs from both withdrawal projections")
	}
	cached.ForwardingHash = forwardingRenderHash(forwarding.next.Data)
	if err := validateTransitAdvertisementCache(cached, cfg, speaker, &forwarding.next); err != nil {
		return nil, err
	}
	return &withdrawnAdvertisementCache{path: path, raw: raw, next: cached}, nil
}

func readPrivateRetirementCache(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("retirement cache must be a private regular file")
	}
	return os.ReadFile(path)
}

// Caller has verified native absence and re-admitted the withdrawal under the
// host lock. A BGP write failure leaves forwarding unchanged. A later forwarding
// write failure leaves the exact next BGP projection for safe admitted retry.
func commitWithdrawnSiteCaches(forwarding *withdrawnForwardingCache, advertisements *withdrawnAdvertisementCache) error {
	if advertisements == nil {
		return commitWithdrawnForwardingCache(forwarding)
	}
	if forwarding == nil {
		return fmt.Errorf("advertisement commit requires forwarding projection")
	}
	raw, err := readPrivateRetirementCache(forwarding.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, forwarding.raw) {
		return fmt.Errorf("forwarding cache changed before advertisement commit")
	}
	raw, err = readPrivateRetirementCache(advertisements.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, advertisements.raw) {
		return fmt.Errorf("advertisement cache changed during withdrawal")
	}
	if err := writeDurableJSON(advertisements.path, advertisements.next); err != nil {
		return err
	}
	return commitWithdrawnForwardingCache(forwarding)
}
