package provider

import (
	"net"
	"net/http"
	"time"
)

// NewHTTPClient returns a client for upstream calls. It has no overall Timeout,
// because http.Client.Timeout would cut off a stream that runs for minutes.
// firstByte bounds only the wait for the response headers. Some providers send
// headers before the first token, so the first-token limit is the Watchdog's.
func NewHTTPClient(connect, firstByte time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() // keeps the proxy, HTTP/2 and pool defaults
	t.DialContext = (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = firstByte
	return &http.Client{Transport: t}
}
