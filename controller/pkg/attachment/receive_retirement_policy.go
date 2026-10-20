package attachment

import (
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// ReceiveRetirementReceiptAge selects the immutable journal's freshness bound.
// The configured policy is an admission ceiling, never permission to extend
// observations from an older journal. Lowering it below the journal's bound
// fails closed rather than changing the meaning of durable retirement evidence.
func ReceiveRetirementReceiptAge(snapshot handover.Snapshot, ceiling time.Duration) (time.Duration, error) {
	if ceiling <= 0 {
		return 0, fmt.Errorf("retirement receipt policy requires a positive ceiling")
	}
	view, err := snapshot.View()
	if err != nil {
		return 0, err
	}
	age := view.Round.MaxReceiptAge
	if age <= 0 || age > ceiling {
		return 0, fmt.Errorf("journal retirement receipt age exceeds configured policy")
	}
	return age, nil
}
