package handover

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ReceiptClient trusts only the controller CA supplied through admitted
// configuration. Redirects and environment proxies cannot receive credentials.
// Tokens are supplied per call so projected credential rotation is respected.
type ReceiptClient struct {
	endpoint string
	client   *http.Client
}

func NewReceiptClient(endpoint string, caPEM []byte) (*ReceiptClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("receipt endpoint requires a direct HTTPS URL")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("receipt endpoint requires a trusted CA")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return &ReceiptClient{endpoint: endpoint, client: &http.Client{
		Transport: transport, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *ReceiptClient) Close() { c.client.CloseIdleConnections() }

// Submit returns after the controller accepts observations or commits the next
// round. Neither response authorizes native actions: agents must reload and
// admit the authoritative journal before executing any subsequent phase.
func (c *ReceiptClient) Submit(ctx context.Context, bearer string, request ReceiptRequest) error {
	if bearer == "" || len(bearer) > 16384 || strings.ContainsAny(bearer, " \t\r\n") {
		return fmt.Errorf("receipt credential is missing or invalid")
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > 1<<20 {
		return fmt.Errorf("receipt request cannot be encoded within the transport limit")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("receipt request could not be constructed")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+bearer)
	response, err := c.client.Do(r)
	if err != nil {
		// Do not propagate request, URL, header or arbitrary peer error text.
		return fmt.Errorf("receipt delivery failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("receipt controller rejected submission (HTTP %d)", response.StatusCode)
	}
	return nil
}
