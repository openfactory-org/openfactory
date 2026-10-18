// Package runnertoken makes and checks the tokens that a runner uses with
// the Open Factory control plane.
//
// A session token (prefix "ofs_") authenticates one session of one run. It
// holds signed claims and is checked with HMAC-SHA-256 and a key ID, so the
// server can check it without a database lookup.
// A bootstrap token (prefix "ofb_") starts a session. It is 32 random bytes,
// and the server keeps only a hash of it.
// The package stores nothing and has no network code.
package runnertoken
