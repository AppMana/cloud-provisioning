package handover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ReceiptRequest contains observations from one agent. Its pod credential is
// sent only in the Authorization header, over directly terminated TLS.
type ReceiptRequest struct {
	MachineUID string       `json:"machineUID"`
	RoundHash  string       `json:"roundHash"`
	Receipts   []PodReceipt `json:"receipts"`
}

// ReceiptResolver must admit the current reserved journal and public plan,
// including live mesh/machine identities. Policies must come from trusted
// release configuration, never from the request or a submitting pod.
type ReceiptResolver func(context.Context, string) (Store, map[string]PodReceiptAuthenticator, error)

type receiptBatch struct {
	uid, round string
	expires    time.Time
	maxAge     time.Duration
	entries    map[[2]string]PodReceipt
}

// Prune each credential at its own observation deadline. Expiring one member
// must not discard newer authenticated observations from other participants.
func (b receiptBatch) prune(now time.Time) bool {
	if !now.Before(b.expires) || b.maxAge <= 0 {
		clear(b.entries)
		return false
	}
	for key, entry := range b.entries {
		if !now.Before(entry.Receipt.ObservedAt.Add(b.maxAge)) {
			delete(b.entries, key)
		}
	}
	return len(b.entries) > 0
}

// ReceiptHandler collects independently authenticated observations. Credentials
// live only in bounded process memory until expiry, failure or advancement.
// Restarting this handler requires agents to resubmit; persisted observations
// alone never authorize advancement. A complete batch is authenticated again.
// Run alongside a server with TLS, header/body timeouts and bounded headers.
// Each controller process may have its own collector: journal CAS remains the
// authority, and no conflict is retried with an already assembled batch.
type ReceiptHandler struct {
	resolve ReceiptResolver
	limit   int
	gate    chan struct{}
	batches map[string]receiptBatch
	stopped bool
}

func NewReceiptHandler(resolve ReceiptResolver, maxPending int) (*ReceiptHandler, error) {
	if resolve == nil || maxPending < 1 {
		return nil, fmt.Errorf("receipt transport requires admission and a positive pending limit")
	}
	return &ReceiptHandler{resolve: resolve, limit: maxPending, gate: make(chan struct{}, 1), batches: map[string]receiptBatch{}}, nil
}

// Run expires credentials even when agents stop sending, and drops every
// pending submission when the owning controller stops or loses leadership.
func (h *ReceiptHandler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.gate <- struct{}{}
			clear(h.batches)
			h.stopped = true
			<-h.gate
			return
		case now := <-ticker.C:
			select {
			case h.gate <- struct{}{}:
				for key, batch := range h.batches {
					if !batch.prune(now) {
						delete(h.batches, key)
					}
				}
				<-h.gate
			default:
			}
		}
	}
}

func (h *ReceiptHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	reject := func(status int) { http.Error(w, "receipt submission rejected", status) }
	if r.TLS == nil {
		reject(http.StatusUpgradeRequired)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		reject(http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") {
		reject(http.StatusUnauthorized)
		return
	}
	bearer := strings.TrimPrefix(auth[0], "Bearer ")
	if bearer == "" || len(bearer) > 16384 || strings.ContainsAny(bearer, " \t\r\n") {
		reject(http.StatusUnauthorized)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request ReceiptRequest
	if err := decoder.Decode(&request); err != nil {
		reject(http.StatusBadRequest)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || !token(request.MachineUID) || !hexToken(request.RoundHash, 32) || len(request.Receipts) == 0 {
		reject(http.StatusBadRequest)
		return
	}
	first := request.Receipts[0]
	for i := range request.Receipts {
		entry := &request.Receipts[i]
		if entry.Receipt.NodeUID != first.Receipt.NodeUID || entry.BootID != first.BootID || entry.Receipt.AgentContainerID != first.Receipt.AgentContainerID || entry.Receipt.RoundHash != request.RoundHash {
			reject(http.StatusBadRequest)
			return
		}
		entry.BearerToken = bearer
	}
	// Do not build an unbounded queue retaining bearer tokens during API stalls.
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	default:
		reject(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if h.stopped {
		reject(http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	for key, batch := range h.batches {
		if !batch.prune(now) {
			delete(h.batches, key)
		}
	}
	store, policies, err := h.resolve(r.Context(), request.MachineUID)
	if err != nil {
		delete(h.batches, request.MachineUID)
		reject(http.StatusConflict)
		return
	}
	snapshot, err := store.Load(r.Context())
	if err != nil {
		delete(h.batches, request.MachineUID)
		reject(http.StatusConflict)
		return
	}
	store.ExpectedUID = string(snapshot.Metadata().UID)
	view, err := snapshot.Transition().View()
	if err != nil {
		delete(h.batches, request.MachineUID)
		reject(http.StatusConflict)
		return
	}
	digest, err := view.Round.Digest()
	if err != nil || digest != request.RoundHash || view.Complete || now.Before(view.Round.IssuedAt) || !now.Before(view.Round.ExpiresAt) {
		reject(http.StatusConflict)
		return
	}
	batch := h.batches[request.MachineUID]
	if batch.uid != string(snapshot.Metadata().UID) || batch.round != digest {
		batch = receiptBatch{uid: string(snapshot.Metadata().UID), round: digest, expires: view.Round.ExpiresAt, maxAge: view.Round.MaxReceiptAge, entries: map[[2]string]PodReceipt{}}
	}
	if _, exists := h.batches[request.MachineUID]; !exists && len(h.batches) >= h.limit {
		reject(http.StatusServiceUnavailable)
		return
	}
	// Authenticate and commit this subset before retaining its credentials.
	// Store validates membership, duplicates, freshness and exact key bindings.
	observed, err := store.ObserveWithPodReceipts(r.Context(), digest, request.Receipts, policies)
	if err != nil || string(observed.Metadata().UID) != batch.uid {
		delete(h.batches, request.MachineUID)
		reject(http.StatusConflict)
		return
	}
	batch.prune(time.Now())
	for _, entry := range request.Receipts {
		batch.entries[[2]string{entry.Receipt.NodeUID, entry.Receipt.DeviceID}] = entry
	}
	h.batches[request.MachineUID] = batch
	if len(batch.entries) != len(view.Round.Members) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	all := make([]PodReceipt, 0, len(batch.entries))
	for _, member := range view.Round.Members {
		all = append(all, batch.entries[[2]string{member.NodeUID, member.DeviceID}])
	}
	delete(h.batches, request.MachineUID)
	if _, err := store.AdvanceWithPodReceipts(r.Context(), digest, all, policies); err != nil {
		reject(http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
