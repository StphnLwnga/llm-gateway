package provider

import (
	"net"
	"net/http"
	"time"
)

// NewHTTPClient returns a client for upstream calls. It has no overall Timeout,
// because http.Client.Timeout would cut off a stream that runs for minutes.
// firstByte bounds the wait for the response headers; for a streamed answer
// that includes model load and prompt processing.
func NewHTTPClient(connect, firstByte time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() // keeps the proxy, HTTP/2 and pool defaults
	t.DialContext = (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = firstByte
	return &http.Client{Transport: t}
}
