package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// NewStreamingHandler forwards requests to the validated, non-nil target without a total deadline.
// transport is caller-owned and must bound connection establishment and response-header waits.
// idleTimeout bounds each wait for more upstream body bytes.
func NewStreamingHandler(target *url.URL, transport http.RoundTripper, idleTimeout time.Duration) http.Handler {
	forwarder := newForwarder(target, transport)
	// ReverseProxy copies backend body bytes to the client. Immediate flushing lets
	// the client receive each copied chunk while backend generation is still running.
	forwarder.FlushInterval = -1
	return &streamHandler{forwarder: forwarder, idleTimeout: idleTimeout}
}

type streamHandler struct {
	forwarder   *httputil.ReverseProxy
	idleTimeout time.Duration
}

func (h *streamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The validated chat request enters from the gateway. Keep client cancellation
	// and let a stalled upstream read cancel this request without limiting total stream time.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Each request owns its proxy copy and cancellation wrapper; the underlying
	// transport remains shared. The rewritten request then travels to the backend.
	forwarder := *h.forwarder
	forwarder.Transport = &idleTransport{
		base:        h.forwarder.Transport,
		cancel:      cancel,
		idleTimeout: h.idleTimeout,
	}
	forwarder.ServeHTTP(w, r.WithContext(ctx))
}

type idleTransport struct {
	base        http.RoundTripper
	cancel      context.CancelFunc
	idleTimeout time.Duration
}

func (t *idleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return response, err
	}
	// The transport returns backend headers and a body reader. ReverseProxy reads
	// through this wrapper before flushing those bytes to the gateway client.
	response.Body = &idleBody{
		ReadCloser:  response.Body,
		cancel:      t.cancel,
		idleTimeout: t.idleTimeout,
	}
	return response, nil
}

type idleBody struct {
	io.ReadCloser
	cancel      context.CancelFunc
	idleTimeout time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	finished := make(chan struct{})
	timer := time.AfterFunc(b.idleTimeout, func() {
		b.cancel()
		close(finished)
	})
	// Stop the timer after every read. If its callback started, wait for completion
	// so this request never leaves a cancellation callback behind.
	defer func() {
		if !timer.Stop() {
			<-finished
		}
	}()
	return b.ReadCloser.Read(p)
}
