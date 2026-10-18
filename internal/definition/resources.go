package definition

import (
	"net/textproto"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

var macShapes = map[[2]int]bool{{4, 7}: true, {6, 14}: true, {8, 14}: true, {12, 28}: true, {12, 56}: true}

var macVersions = []string{"14", "15", "26", "27"}

// DefaultMacVersion is used when a macOS machine omits platform.mac.
const DefaultMacVersion = "26"

func isPowerOfTwo(n int) bool { return n > 0 && n&(n-1) == 0 }

func parseMachine(d *diag, file, name, content string) (core.MachineSpec, bool) {
	c := &doc{d: d, file: file}
	before := len(d.errs)
	spec := core.MachineSpec{Name: name}
	root := c.parse(content)
	if root == nil {
		return spec, false
	}
	m := c.mapping(root, "")
	if m == nil {
		return spec, false
	}
	defer m.finish()
	if v := m.get("description"); v != nil && !isNull(v) {
		spec.Description, _ = c.str(v, "description")
	}
	if v := m.get("setupCommands"); v != nil && !isNull(v) {
		spec.SetupCommands, _ = c.strList(v, "setupCommands")
	}
	if v := m.get("failureSessionRetentionMinutes"); v != nil && !isNull(v) {
		spec.FailureSessionRetentionMinutes, _ = c.intRange(v, "failureSessionRetentionMinutes", 1, 60)
	}
	shapeSet := false
	var shapeNode = m.get("instanceShape")
	if shapeNode != nil && !isNull(shapeNode) {
		if sm := c.mapping(shapeNode, "instanceShape"); sm != nil {
			if !sm.present("vcpus") || !sm.present("memoryGb") {
				c.errf(sm.node, "instanceShape", "instanceShape must set both vcpus and memoryGb")
			}
			if v := sm.get("vcpus"); v != nil && !isNull(v) {
				spec.VCPUs, _ = c.integer(v, "instanceShape.vcpus")
			}
			if v := sm.get("memoryGb"); v != nil && !isNull(v) {
				spec.MemoryGB, _ = c.integer(v, "instanceShape.memoryGb")
			}
			sm.finish()
			shapeSet = spec.VCPUs != 0 || spec.MemoryGB != 0
		}
	}

	pv := m.require("platform")
	if pv == nil {
		return spec, false
	}
	pm := c.mapping(pv, "platform")
	if pm == nil {
		return spec, false
	}
	defer pm.finish()
	spec.OS = "linux"
	if v := pm.get("os"); v != nil && !isNull(v) {
		if s, ok := c.enum(v, "platform.os", "linux", "macos", "windows"); ok {
			spec.OS = s
		}
	}
	switch spec.OS {
	case "macos":
		spec.Arch = "aarch64"
	default:
		spec.Arch = "x86_64"
	}
	if v := pm.get("arch"); v != nil && !isNull(v) {
		if s, ok := c.enum(v, "platform.arch", "x86_64", "aarch64"); ok {
			switch {
			case spec.OS == "macos" && s != "aarch64":
				c.errf(v, "platform.arch", "macOS machines support only aarch64")
			case spec.OS == "windows" && s != "x86_64":
				c.errf(v, "platform.arch", "Windows machines support only x86_64")
			}
			spec.Arch = s
		}
	}
	linux := pm.get("linux")
	mac := pm.get("mac")
	if linux != nil && !isNull(linux) && spec.OS != "linux" {
		c.errf(linux, "platform.linux", "platform.linux is only valid when platform.os is linux")
	}
	if mac != nil && !isNull(mac) && spec.OS != "macos" {
		c.errf(mac, "platform.mac", "platform.mac is only valid when platform.os is macos")
	}
	switch spec.OS {
	case "linux":
		if linux == nil || isNull(linux) {
			c.errf(pm.node, "platform.linux.image", "Linux machines require platform.linux.image")
			break
		}
		if lm := c.mapping(linux, "platform.linux"); lm != nil {
			if v := lm.require("image"); v != nil {
				if s, ok := c.nonEmptyStr(v, "platform.linux.image"); ok {
					if strings.ContainsAny(s, " \t") {
						c.errf(v, "platform.linux.image", "platform.linux.image %q is not a valid image reference", s)
					}
					spec.Image = s
				}
			}
			if v := lm.get("registryCredentialSecretName"); v != nil && !isNull(v) {
				spec.RegistryCredentialSecretName, _ = c.secretName(v, "platform.linux.registryCredentialSecretName")
			}
			if v := lm.get("user"); v != nil && !isNull(v) {
				spec.User = c.linuxUser(v)
			}
			lm.finish()
		}
	case "macos":
		spec.MacVersion = DefaultMacVersion
		if mac != nil && !isNull(mac) {
			if mm := c.mapping(mac, "platform.mac"); mm != nil {
				if v := mm.get("version"); v != nil && !isNull(v) {
					if s, ok := c.enum(v, "platform.mac.version", macVersions...); ok {
						spec.MacVersion = s
					}
				}
				mm.finish()
			}
		}
	}
	if shapeSet {
		switch spec.OS {
		case "macos":
			if !macShapes[[2]int{spec.VCPUs, spec.MemoryGB}] {
				c.errf(shapeNode, "instanceShape", "macOS instanceShape must be one of 4 vCPUs/7 GB, 6/14, 8/14, 12/28 or 12/56 (got %d/%d)", spec.VCPUs, spec.MemoryGB)
			}
		default:
			if !isPowerOfTwo(spec.VCPUs) || !isPowerOfTwo(spec.MemoryGB) {
				c.errf(shapeNode, "instanceShape", "%s instanceShape vcpus and memoryGb must both be positive powers of two (got %d/%d)", spec.OS, spec.VCPUs, spec.MemoryGB)
			}
		}
	}
	return spec, len(d.errs) == before
}

// SignatureSchemes are the provider signature schemes a webhook can verify.
var SignatureSchemes = []string{"github", "pagerduty", "sentry", "standard_webhooks", "stripe", "vercel"}

// credentialHeaders cannot be used as a webhook delivery ID header.
var credentialHeaders = map[string]bool{
	"authorization":         true,
	"proxy-authorization":   true,
	"cookie":                true,
	"set-cookie":            true,
	"x-api-key":             true,
	"x-hub-signature":       true,
	"x-hub-signature-256":   true,
	"x-pagerduty-signature": true,
	"sentry-hook-signature": true,
	"stripe-signature":      true,
	"x-vercel-signature":    true,
	"webhook-signature":     true,
	"svix-signature":        true,
	"x-gitlab-token":        true,
	"x-slack-signature":     true,
}

// MaxWebhooks is the maximum number of webhooks per factory.
const MaxWebhooks = 20

// ValidDeliveryIDHeader reports whether h may be used as a webhook delivery ID header.
func ValidDeliveryIDHeader(h string) (bool, string) {
	if h == "" || len(h) > 64 {
		return false, "must be 1-64 characters"
	}
	for i := 0; i < len(h); i++ {
		if !isTokenChar(h[i]) {
			return false, "is not a valid HTTP header name"
		}
	}
	if credentialHeaders[strings.ToLower(h)] {
		return false, "is a credential-bearing header and cannot identify deliveries"
	}
	return true, ""
}

func isTokenChar(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
}

// ValidURLTokenSecret reports whether a secret value can be embedded in a
// url_token webhook URL (letters, digits, '-', '.', '_' and '~').
func ValidURLTokenSecret(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		b := v[i]
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '-' && b != '.' && b != '_' && b != '~' {
			return false
		}
	}
	return true
}

func parseWebhook(d *diag, file, name, content string) (core.Webhook, bool) {
	c := &doc{d: d, file: file}
	before := len(d.errs)
	w := core.Webhook{Name: name, AuthMode: "token", Enabled: true}
	root := c.parse(content)
	if root == nil {
		return w, false
	}
	m := c.mapping(root, "")
	if m == nil {
		return w, false
	}
	defer m.finish()
	if v := m.get("authMode"); v != nil && !isNull(v) {
		if s, ok := c.enum(v, "authMode", "token", "url_token", "signature"); ok {
			w.AuthMode = s
		}
	}
	sv := m.get("signatureScheme")
	switch {
	case w.AuthMode == "signature" && (sv == nil || isNull(sv)):
		c.errf(m.node, "signatureScheme", "signatureScheme is required when authMode is signature")
	case w.AuthMode != "signature" && sv != nil && !isNull(sv):
		c.errf(sv, "signatureScheme", "signatureScheme is only valid when authMode is signature")
	case sv != nil && !isNull(sv):
		w.SignatureScheme, _ = c.enum(sv, "signatureScheme", SignatureSchemes...)
	}
	if v := m.require("secretName"); v != nil {
		w.SecretName, _ = c.secretName(v, "secretName")
	}
	if v := m.get("deliveryIdHeader"); v != nil && !isNull(v) {
		if s, ok := c.str(v, "deliveryIdHeader"); ok {
			if good, why := ValidDeliveryIDHeader(s); !good {
				c.errf(v, "deliveryIdHeader", "deliveryIdHeader %q %s", s, why)
			} else {
				w.DeliveryIDHeader = textproto.CanonicalMIMEHeaderKey(s)
			}
		}
	}
	if v := m.get("enabled"); v != nil && !isNull(v) {
		if b, ok := c.boolean(v, "enabled"); ok {
			w.Enabled = b
		}
	}
	return w, len(d.errs) == before
}

// linuxUser reads platform.linux.user: a number of 1 or more, or "image".
// Root (0) is refused: the sandbox user must not be root.
func (c *doc) linuxUser(v *yaml.Node) string {
	const path = "platform.linux.user"
	if v.Kind == yaml.ScalarNode && v.Value == "image" {
		return "image"
	}
	if r := resolve(v); r == nil || r.Kind != yaml.ScalarNode || r.Tag != "!!int" {
		c.errf(v, path, "%s must be a numeric user ID of 1 or more, or image", path)
		return ""
	}
	n, ok := c.integer(v, path)
	if !ok {
		return ""
	}
	if n < 1 {
		c.errf(v, path, "%s must be a numeric user ID of 1 or more, or image (got %d)", path, n)
		return ""
	}
	return strconv.Itoa(n)
}
