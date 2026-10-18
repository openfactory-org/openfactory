package core

// Reasons of an InterruptError.
const (
	// InterruptNoActiveTurn means that the run has no active turn.
	InterruptNoActiveTurn = "no_active_turn"
	// InterruptUnsupportedHarness means that the harness cannot interrupt a turn.
	InterruptUnsupportedHarness = "unsupported_harness"
)

// InterruptError is returned when a run cannot be interrupted. It is a
// conflict, and Reason tells the caller why.
type InterruptError struct{ Reason string }

func (e *InterruptError) Error() string { return "interrupt: " + e.Reason }

// Unwrap makes errors.Is(err, ErrConflict) true.
func (e *InterruptError) Unwrap() error { return ErrConflict }
