// Package upstream owns every outbound HTTP call MCPaw makes.
//
// Centralising egress here means the SSRF policy, timeouts, response caps,
// circuit breaking and rate limiting are applied uniformly and cannot be
// bypassed by a new call site that forgets one of them.
package upstream

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// EgressPolicy describes what an instance is permitted to talk to.
//
// The zero value denies every private destination, which is the correct default
// for a service that fetches URLs on behalf of a language model.
type EgressPolicy struct {
	// AllowPrivateNetworks permits loopback, RFC1918, carrier-grade NAT and
	// IPv6 unique-local destinations. It exists because genuinely local APIs —
	// the Zotero desktop app being the motivating case — live there. It is an
	// explicit, audited, per-instance opt-in.
	AllowPrivateNetworks bool
}

// blockedAlways are ranges that no policy may unblock.
//
// Link-local is in this list on purpose. It is where cloud instance metadata
// services live (169.254.169.254), and those endpoints hand out credentials to
// anyone who can make an HTTP request from the instance. An operator enabling
// private-network egress wants to reach their own LAN or their host machine —
// they are not asking to expose the platform's own cloud credentials.
var blockedAlways = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this" network
	netip.MustParsePrefix("169.254.0.0/16"), // IPv4 link-local + cloud metadata
	netip.MustParsePrefix("224.0.0.0/4"),    // multicast
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, includes broadcast
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("::/128"),         // unspecified
	netip.MustParsePrefix("fe80::/10"),      // IPv6 link-local
	netip.MustParsePrefix("ff00::/8"),       // IPv6 multicast
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use IPv4/IPv6 translation
	netip.MustParsePrefix("100::/64"),       // discard-only
}

// blockedUnlessPrivateAllowed are the ranges the per-instance opt-in unlocks.
var blockedUnlessPrivateAllowed = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),    // IPv4 loopback
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("::1/128"),        // IPv6 loopback
	netip.MustParsePrefix("fc00::/7"),       // IPv6 unique local
}

// BlockedIPError reports a destination refused by the egress policy. It carries
// the address so the audit log can record exactly what was attempted.
type BlockedIPError struct {
	IP     netip.Addr
	Reason string
}

// Error implements error.
func (e *BlockedIPError) Error() string {
	return fmt.Sprintf("egress to %s refused: %s", e.IP, e.Reason)
}

// CheckIP applies the egress policy to a resolved address.
func CheckIP(ip netip.Addr, policy EgressPolicy) error {
	if !ip.IsValid() {
		return &BlockedIPError{IP: ip, Reason: "not a valid IP address"}
	}
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) is the same host as its
	// IPv4 form. Unmapping first is what stops that notation from being used to
	// slip past the prefix checks.
	ip = ip.Unmap()

	for _, p := range blockedAlways {
		if prefixContains(p, ip) {
			return &BlockedIPError{IP: ip, Reason: "address is in a permanently blocked range " + p.String()}
		}
	}
	if policy.AllowPrivateNetworks {
		return nil
	}
	for _, p := range blockedUnlessPrivateAllowed {
		if prefixContains(p, ip) {
			return &BlockedIPError{
				IP: ip,
				Reason: "address is private (" + p.String() +
					") and this instance does not have private-network egress enabled",
			}
		}
	}
	return nil
}

// prefixContains compares only address families that match, so an IPv4 prefix
// never accidentally matches an IPv6 address or vice versa.
func prefixContains(p netip.Prefix, ip netip.Addr) bool {
	if p.Addr().Is4() != ip.Is4() {
		return false
	}
	return p.Contains(ip)
}

// CheckAddress applies the policy to a "host:port" pair that has already been
// resolved to an IP literal.
func CheckAddress(address string, policy EgressPolicy) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress: cannot parse address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// Reaching here means the dialer was handed a name rather than an
		// address, which would mean resolution had not happened yet and the
		// check would be meaningless. Refuse instead of assuming.
		return fmt.Errorf("egress: %q is not a resolved IP address", host)
	}
	return CheckIP(ip, policy)
}

// controlFunc returns a dialer Control hook enforcing the policy.
//
// This is the load-bearing SSRF control. Checking the hostname before dialling
// is not enough: an attacker who controls DNS can answer the pre-flight lookup
// with a public address and the real connection's lookup with 127.0.0.1 (DNS
// rebinding). Control runs after resolution with the address actually being
// connected to, so there is no window between check and use.
func controlFunc(policy EgressPolicy) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			return fmt.Errorf("egress: network %q is not permitted", network)
		}
		return CheckAddress(address, policy)
	}
}
