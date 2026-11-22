package main

import "testing"

// The report warning fires only for a wildcard or unparsable address, not
// for the tap address that the microVM worker host gives each VM.
func TestSpecificAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:3129":  true,
		"[::1]:3129":      true,
		"10.213.0.1:3129": true,
		"0.0.0.0:3129":    false,
		"[::]:3129":       false,
		":3129":           false,
		"localhost:3129":  false,
		"bad":             false,
	} {
		if got := specificAddr(addr); got != want {
			t.Errorf("specificAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
