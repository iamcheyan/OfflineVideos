package provider

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// newResolver returns a resolver that works on hosts whose system resolver
// cannot be used.
//
// Android's libc forwards getaddrinfo to netd on [::1]:53. That socket is
// reachable from ordinary apps but returns ECONNREFUSED from inside a Termux
// process started via su, which is exactly how videod runs on this device.
// Go's pure resolver has the same problem because it still reads the system
// configuration first. Pointing at a public resolver by address sidesteps the
// stub entirely; the address is configurable so a user behind a filtering
// network can supply their own.
func newResolver() *net.Resolver {
	server := os.Getenv("POCKET_DNS_SERVER")
	if server == "" {
		return net.DefaultResolver
	}
	if !strings.Contains(server, ":") {
		server += ":53"
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", server)
		},
	}
}

// resolver is the shared instance used by direct provider HTTP calls.
var resolver = newResolver()
