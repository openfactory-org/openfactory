// Package runnerclient is the client that a runner process uses to talk to
// the control plane of Open Factory. It follows the runner protocol in
// package runnerproto.
//
// The Client type sends one HTTP request for each protocol call. It does no
// retries: the caller decides what to do after an error. The EventLog type is
// a durable, append-only log of events in one local file. The Sender type
// sends the events of the log to the control plane and keeps sending until
// the control plane has all of them.
package runnerclient
