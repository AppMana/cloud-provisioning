package check

// Details preserves each probe result alongside aggregate convergence status.
func (m *Matrix) Details() map[string]any {
	var results []map[string]any
	for _, r := range m.Results() {
		detail := map[string]any{"from": r.From, "to": r.To, "kind": r.Kind, "passed": r.OK, "detail": r.Detail, "notRequired": r.NotRequired}
		if r.Err != nil {
			detail["error"] = r.Err.Error()
		}
		results = append(results, detail)
	}
	return map[string]any{"total": m.Total(), "passed": m.Passed(), "failed": m.Failed(), "notRequired": m.NotRequired(), "elapsed": m.Elapsed.String(), "cancelled": m.Cancelled, "results": results}
}
