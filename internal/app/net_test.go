package app

import (
	"net"
	"testing"
)

func TestPublicURLForWildcardAddrUsesLANIPv4(t *testing.T) {
	t.Parallel()

	got := publicURLForAddr("0.0.0.0:8765", func() ([]net.Addr, error) {
		return []net.Addr{
			ipNet("127.0.0.1"),
			ipNet("169.254.1.2"),
			ipNet("192.168.1.42"),
		}, nil
	})
	if got != "http://192.168.1.42:8765" {
		t.Fatalf("publicURLForAddr wildcard = %q, want LAN URL", got)
	}
}

func TestPublicURLForWildcardAddrFallsBackToLoopback(t *testing.T) {
	t.Parallel()

	got := publicURLForAddr(":8765", func() ([]net.Addr, error) {
		return []net.Addr{ipNet("127.0.0.1")}, nil
	})
	if got != "http://127.0.0.1:8765" {
		t.Fatalf("publicURLForAddr fallback = %q, want loopback URL", got)
	}
}

func TestLoopbackAddrForLocalConnectNormalizesWildcardToLoopback(t *testing.T) {
	t.Parallel()

	if got := LoopbackAddrForLocalConnect("0.0.0.0:8765"); got != "127.0.0.1:8765" {
		t.Fatalf("LoopbackAddrForLocalConnect wildcard = %q, want loopback", got)
	}
	if got := LoopbackAddrForLocalConnect("192.168.1.42:8765"); got != "192.168.1.42:8765" {
		t.Fatalf("LoopbackAddrForLocalConnect concrete = %q, want concrete host preserved", got)
	}
}

func TestDefaultAddrIsLANReachableWildcard(t *testing.T) {
	t.Parallel()

	if DefaultAddr != "0.0.0.0:8765" {
		t.Fatalf("DefaultAddr = %q, want wildcard LAN/dev bind", DefaultAddr)
	}
}

func ipNet(raw string) *net.IPNet {
	return &net.IPNet{IP: net.ParseIP(raw), Mask: net.CIDRMask(24, 32)}
}
