package provider

import (
	"net"
	"net/http"
	"time"
)

// httpClient is used for Bilibili API calls.
//
// A custom transport is required rather than http.DefaultClient so the dialer
// can use the resolver from dns.go. Timeouts are explicit because a stuck DNS
// lookup with no deadline would hang a sync request until the caller's context
// expires.
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Resolver:  resolver,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}
