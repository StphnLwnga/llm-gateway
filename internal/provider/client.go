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
	t := http.DefaultTransport.(*http.Transport).Clone() // keeps the proxy and HTTP/2 defaults
	// Go's default of 2 idle per host makes concurrent HTTP/1.1 upstreams (Ollama, vLLM) dial on most requests.
	t.MaxIdleConnsPerHost = 256
	t.MaxIdleConns = 1024
	t.DialContext = (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = firstByte
	return &http.Client{Transport: t}
}
