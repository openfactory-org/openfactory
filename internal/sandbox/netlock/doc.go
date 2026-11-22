// Package netlock is the in-sandbox egress lock. A hosted sandbox provider
// that cannot enforce a network policy itself runs the runner as root, and
// the runner locks the network before any agent code runs:
//
//  1. It creates the users ofproxy and agent (uid 1001).
//  2. It starts netguard as ofproxy. netguard enforces the policy of the run.
//  3. It applies iptables and ip6tables OUTPUT rules: ofproxy may go out;
//     agent may use loopback, except the daemon ports of the provider;
//     every other TCP or UDP packet of agent is rejected.
//  4. It probes direct, by-IP and daemon-port connections as agent. If a
//     probe does not give the expected result, the lock fails closed.
//  5. It runs the harness, the setup commands and every git command in the
//     workspace as agent, with the proxy variables set.
//
// The mode comes from OF_EGRESS_LOCK: "none" (no lock) or "iptables". The
// value "netns" is reserved for a network namespace lock.
package netlock
