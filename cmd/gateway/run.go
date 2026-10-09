package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
)

func run(ctx context.Context) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("load gateway config: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: cfg.upstreamConnectTimeout}).DialContext
	transport.TLSHandshakeTimeout = cfg.upstreamConnectTimeout
	transport.ResponseHeaderTimeout = cfg.upstreamHeaderTimeout
	defer transport.CloseIdleConnections()
	var nonStreaming http.Handler
	var streaming http.Handler
	if cfg.upstreamURL != nil {
		nonStreaming = proxy.NewNonStreamingHandler(cfg.upstreamURL, transport, cfg.upstreamTimeout)
		streaming = proxy.NewStreamingHandler(cfg.upstreamURL, transport, cfg.streamIdleTimeout)
	}
	handler := gateway.NewHandlerWithBackends(nonStreaming, streaming, gateway.ChatLimits{
		MaxBodyBytes: cfg.maxBodyBytes,
		MaxTokens:    cfg.maxTokens,
	})

	// Bound request reads and idle connections without limiting response streams.
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadTimeout:       cfg.readTimeout,
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		IdleTimeout:       cfg.idleTimeout,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen gateway HTTP on %s: %w", server.Addr, err)
	}
	if err := runServer(ctx, server, listener, cfg.shutdownTimeout); err != nil {
		return fmt.Errorf("run gateway HTTP on %s: %w", server.Addr, err)
	}
	return nil
}
