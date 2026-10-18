// Package runstate decides which state a run goes to when an event happens.
//
// The function Next takes the current state and one event with the facts that
// the decision needs. It returns the new state, the blocked reason, the failure code and
// the retry flag.
//
// The package has no side effects. It has no database, no clock and no I/O.
// The caller writes the result and the timeline events.
package runstate
