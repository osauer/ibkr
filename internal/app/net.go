package app

import (
	"net"
	"strings"
)

type interfaceAddrsFunc func() ([]net.Addr, error)

// PublicURLForAddr returns the browser-visible URL for an app listen address.
// Wildcard binds need a concrete LAN address because phones cannot resolve the
// Mac's loopback address.
func PublicURLForAddr(addr string) string {
	return publicURLForAddr(addr, net.InterfaceAddrs)
}

func publicURLForAddr(addr string, addrs interfaceAddrsFunc) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if isWildcardHost(host) {
		if lan := firstLANIPv4(addrs); lan != "" {
			host = lan
		} else {
			host = "127.0.0.1"
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

// LoopbackAddrForLocalConnect returns an address the Mac-side CLI can use to
// reach the local app server. It intentionally differs from PublicURLForAddr.
func LoopbackAddrForLocalConnect(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if isWildcardHost(host) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func isWildcardHost(host string) bool {
	return host == "" || host == "0.0.0.0" || host == "::"
}

func firstLANIPv4(addrs interfaceAddrsFunc) string {
	if addrs == nil {
		return ""
	}
	items, err := addrs()
	if err != nil {
		return ""
	}
	for _, item := range items {
		ip, ok := addrIP(item)
		if !ok || ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

func addrIP(addr net.Addr) (net.IP, bool) {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP, true
	case *net.IPAddr:
		return v.IP, true
	default:
		return nil, false
	}
}
