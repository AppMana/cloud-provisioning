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
	resolve ReceiptCollectionResolver
	limit   int
	gate    chan struct{}
	batches map[string]receiptBatch
	stopped bool
}

func NewReceiptHandler(resolve ReceiptResolver, maxPending int) (*ReceiptHandler, error) {
	if resolve == nil {
		return nil, fmt.Errorf("receipt transport requires admission")
	}
	return NewReceiptCollectionHandler(func(ctx context.Context, machine string) (ReceiptCollection, error) {
		store, policies, err := resolve(ctx, machine)
		if err != nil {
			return ReceiptCollection{}, err
		}
		return NewHandoverReceiptCollection(ctx, store, policies)
	}, maxPending)
}

// NewHandoverReceiptCollection adapts an admitted journal to the shared receipt
// transport. The caller must resolve current source identities and policies.
func NewHandoverReceiptCollection(ctx context.Context, store Store, policies map[string]PodReceiptAuthenticator) (ReceiptCollection, error) {
	snapshot, err := store.Load(ctx)
	if err != nil {
		return ReceiptCollection{}, err
	}
	store.ExpectedUID = string(snapshot.Metadata().UID)
	view, err := snapshot.Transition().View()
	if err != nil || view.Complete {
		return ReceiptCollection{}, fmt.Errorf("receipt round is not active")
	}
	digest, err := view.Round.Digest()
	if err != nil || time.Now().Before(view.Round.IssuedAt) {
		return ReceiptCollection{}, fmt.Errorf("receipt round is not current")
	}
	target := ReceiptCollection{UID: string(snapshot.Metadata().UID), Digest: digest, ExpiresAt: view.Round.ExpiresAt, MaxAge: view.Round.MaxReceiptAge}
	for _, member := range view.Round.Members {
		target.Members = append(target.Members, ReceiptMember{NodeUID: member.NodeUID, DeviceID: member.DeviceID})
	}
	target.Observe = func(ctx context.Context, entries []PodReceipt) error {
		observed, err := store.ObserveWithPodReceipts(ctx, digest, entries, policies)
		if err != nil {
			return err
		}
		if string(observed.Metadata().UID) != target.UID {
			return fmt.Errorf("receipt journal incarnation changed")
		}
		return nil
	}
	target.Complete = func(ctx context.Context, entries []PodReceipt) error {
		_, err := store.AdvanceWithPodReceipts(ctx, digest, entries, policies)
		return err
	}
	return target, nil
}

// ReceiptMember identifies one required observation within a collection.
type ReceiptMember struct{ NodeUID, DeviceID string }

// ReceiptCollection supplies trusted admission and commit operations to the
// existing bounded transport. Observe must authenticate each submitted subset;
// Complete must reauthenticate the full batch and CAS its authoritative state.
// Callbacks must never persist credentials. Members are immutable for a digest.
type ReceiptCollection struct {
	UID, Digest string
	ExpiresAt   time.Time
	MaxAge      time.Duration
	Members     []ReceiptMember
	Observe     func(context.Context, []PodReceipt) error
	Complete    func(context.Context, []PodReceipt) error
}
type ReceiptCollectionResolver func(context.Context, string) (ReceiptCollection, error)

func NewReceiptCollectionHandler(resolve ReceiptCollectionResolver, maxPending int) (*ReceiptHandler, error) {
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
		// No admission or mutation has started. Let agents redeliver this
		// observation within its original lifetime instead of rerunning native
		// cleanup after every collision with another consumer.
		w.Header().Set("Retry-After", "1")
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
	target, err := h.resolve(r.Context(), request.MachineUID)
	if err != nil {
		delete(h.batches, request.MachineUID)
		reject(http.StatusConflict)
		return
	}
	digest := target.Digest
	if !token(target.UID) || !hexToken(digest, 32) || digest != request.RoundHash || !now.Before(target.ExpiresAt) || target.MaxAge <= 0 || len(target.Members) == 0 || target.Observe == nil || target.Complete == nil {
		reject(http.StatusConflict)
		return
	}
	members := map[[2]string]bool{}
	for _, member := range target.Members {
		id := [2]string{member.NodeUID, member.DeviceID}
		if !token(member.NodeUID) || members[id] {
			delete(h.batches, request.MachineUID)
			reject(http.StatusConflict)
			return
		}
		members[id] = true
	}
	submitted := map[[2]string]bool{}
	for _, entry := range request.Receipts {
		id := [2]string{entry.Receipt.NodeUID, entry.Receipt.DeviceID}
		if !members[id] || submitted[id] {
			delete(h.batches, request.MachineUID)
			reject(http.StatusConflict)
			return
		}
		submitted[id] = true
	}
	batch := h.batches[request.MachineUID]
	if batch.uid != target.UID || batch.round != digest {
		batch = receiptBatch{uid: target.UID, round: digest, expires: target.ExpiresAt, maxAge: target.MaxAge, entries: map[[2]string]PodReceipt{}}
	}
	if _, exists := h.batches[request.MachineUID]; !exists && len(h.batches) >= h.limit {
		reject(http.StatusServiceUnavailable)
		return
	}
	// Authenticate this subset before retaining its credentials. The backend
	// validates freshness and exact observation bindings.
	err = target.Observe(r.Context(), request.Receipts)
	if err != nil {
		delete(h.batches, request.MachineUID)
		reject(http.StatusConflict)
		return
	}
	observedAt := time.Now()
	if !observedAt.Before(batch.expires) {
		delete(h.batches, request.MachineUID)
		reject(http.StatusConflict)
		return
	}
	batch.prune(observedAt)
	for _, entry := range request.Receipts {
		batch.entries[[2]string{entry.Receipt.NodeUID, entry.Receipt.DeviceID}] = entry
	}
	h.batches[request.MachineUID] = batch
	if len(batch.entries) != len(target.Members) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	all := make([]PodReceipt, 0, len(batch.entries))
	for _, member := range target.Members {
		all = append(all, batch.entries[[2]string{member.NodeUID, member.DeviceID}])
	}
	delete(h.batches, request.MachineUID)
	if err := target.Complete(r.Context(), all); err != nil {
		reject(http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
