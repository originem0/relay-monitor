package checker

import "fmt"

// DiffEvent represents a detected change between check runs.
type DiffEvent struct {
	Type     string // model_discovered, model_removed, status_changed, provider_state_changed
	Provider string
	Model    string
	OldValue string
	NewValue string
	Message  string
}

// Diff compares current test results against previous results and returns change events.
// previousByModel maps model ID to its previous TestResult-like data.
func Diff(providerName string, current []TestResult, previousModels map[string]PreviousModel) []DiffEvent {
	var events []DiffEvent

	currentSet := make(map[string]bool)
	for _, r := range current {
		currentSet[r.Model] = true
	}

	// Check for new models
	prevSet := make(map[string]bool)
	for model := range previousModels {
		prevSet[model] = true
	}

	for _, r := range current {
		if !prevSet[r.Model] {
			events = append(events, DiffEvent{
				Type:     "model_discovered",
				Provider: providerName,
				Model:    r.Model,
				NewValue: r.Status,
				Message:  fmt.Sprintf("New model discovered: %s", r.Model),
			})
		}
	}

	// Check for removed models
	for model := range previousModels {
		if !currentSet[model] {
			events = append(events, DiffEvent{
				Type:     "model_removed",
				Provider: providerName,
				Model:    model,
				OldValue: "available",
				Message:  fmt.Sprintf("Model removed: %s", model),
			})
		}
	}

	// Check for status changes on existing models
	for _, r := range current {
		prev, exists := previousModels[r.Model]
		if !exists {
			continue
		}

		// Correctness changed
		if r.Correct != prev.Correct {
			old := "wrong"
			new_ := "wrong"
			if prev.Correct {
				old = "correct"
			}
			if r.Correct {
				new_ = "correct"
			}
			events = append(events, DiffEvent{
				Type:     "status_changed",
				Provider: providerName,
				Model:    r.Model,
				OldValue: old,
				NewValue: new_,
				Message:  fmt.Sprintf("%s: %s -> %s", r.Model, old, new_),
			})
		}

		// Status changed (ok vs error)
		if r.Status != prev.Status {
			events = append(events, DiffEvent{
				Type:     "status_changed",
				Provider: providerName,
				Model:    r.Model,
				OldValue: prev.Status,
				NewValue: r.Status,
				Message:  fmt.Sprintf("%s: %s -> %s", r.Model, prev.Status, r.Status),
			})
		}
	}

	return events
}

// PreviousModel holds the key fields from a previous check result for comparison.
type PreviousModel struct {
	Status  string
	Correct bool
}
