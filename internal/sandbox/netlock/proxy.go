package netlock

import (
	"net/url"
	"strconv"
)

// The netguard proxy of a sandbox and its report.
const (
	// ProxyPort is the loopback port of netguard.
	ProxyPort = 3128
	// ReportPort is the loopback port of the report of blocked hosts.
	ReportPort = 3129
	// ReportEnv is the variable with the URL of that report. The runner
	// reads the report and sends one diagnostic for each blocked host.
	ReportEnv = "OF_NETGUARD_REPORT"
	// LocalNoProxy is the part of NO_PROXY that never changes.
	LocalNoProxy = "localhost,127.0.0.1,::1"
)

// ProxyURL is the URL of the netguard proxy on loopback.
func ProxyURL() string { return "http://127.0.0.1:" + strconv.Itoa(ProxyPort) }

// ReportURL is the URL of the report of blocked hosts on loopback.
func ReportURL() string { return "http://127.0.0.1:" + strconv.Itoa(ReportPort) + "/blocked" }

// Env returns the proxy variables for a proxy URL. The hosts in bypass, and
// the loopback addresses, do not use the proxy.
func Env(proxyURL string, bypass ...string) map[string]string {
	noProxy := LocalNoProxy
	for _, h := range bypass {
		if h != "" {
			noProxy = h + "," + noProxy
		}
	}
	return map[string]string{
		"HTTPS_PROXY": proxyURL, "HTTP_PROXY": proxyURL, "https_proxy": proxyURL, "http_proxy": proxyURL,
		"NO_PROXY": noProxy, "no_proxy": noProxy,
	}
}

// ControlViaProxy reports whether a sandbox must reach the control plane
// through the proxy when the lock is on. The lock refuses direct connections
// of the agent, and a control plane on port 80 or 443 is reachable through
// the proxy, which always allows its host.
func ControlViaProxy(controlURL string) bool {
	u, err := url.Parse(controlURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	n, _ := strconv.Atoi(port)
	return n == 80 || n == 443
}
