package rtcore

// EffectiveNetworkPolicy is the merged result. Restricted=false means unrestricted.
type EffectiveNetworkPolicy struct {
	Restricted   bool     `json:"restricted"`
	AllowedHosts []string `json:"allowed_hosts"`
	DeniedHosts  []string `json:"denied_hosts"`
}

// ModeUnrestricted is the only mode of a task policy that gets no egress
// limit. Every other mode, an empty one included, is restricted.
const ModeUnrestricted = "unrestricted"

// Restricted reports whether a task policy limits egress. A nil policy is
// unrestricted.
func Restricted(p *TaskNetworkPolicy) bool {
	return p != nil && p.Mode != ModeUnrestricted
}
