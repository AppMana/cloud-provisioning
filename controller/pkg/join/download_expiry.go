package join

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// validateDownloadExpiry rejects already unusable SigV4 bootstrap URLs. It
// cannot verify a signature or the underlying credential's earlier expiration.
// Errors deliberately omit the URL, whose query contains bearer credentials.
func validateDownloadExpiry(raw string, now time.Time) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid download URL")
	}
	q := u.Query()
	if !q.Has("X-Amz-Date") && !q.Has("X-Amz-Expires") {
		return nil
	}
	if len(q["X-Amz-Date"]) != 1 || len(q["X-Amz-Expires"]) != 1 {
		return fmt.Errorf("ambiguous or incomplete signed download expiry")
	}
	signed, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return fmt.Errorf("invalid signed download timestamp")
	}
	seconds, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || seconds <= 0 || seconds > 604800 {
		return fmt.Errorf("invalid signed download lifetime")
	}
	if !now.Before(signed.Add(time.Duration(seconds) * time.Second)) {
		return fmt.Errorf("signed download URL has expired; renew the configured artifact URL")
	}
	return nil
}
