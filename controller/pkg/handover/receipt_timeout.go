package handover

import "time"

// ReceiptRequestTimeout bounds source admission, queueing, observation and
// completion together. Completion can require multiple live API rechecks;
// it must share the request budget rather than start a new deadline.
const ReceiptRequestTimeout = 30 * time.Second

// ReceiptDeliveryTimeout includes transport and explicit pre-admission busy
// retries. A caller's shorter deadline still wins. This does not extend the
// age of an observation or authorize retrying ambiguous writes.
const ReceiptDeliveryTimeout = ReceiptRequestTimeout + 10*time.Second

// ReceiptWriteTimeout leaves room for request reads and TLS setup before the
// bounded handler runs. The agent's delivery deadline remains shorter.
const ReceiptWriteTimeout = ReceiptDeliveryTimeout + 10*time.Second
