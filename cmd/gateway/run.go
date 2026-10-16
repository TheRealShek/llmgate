package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/therealshek/llmgate/internal/admin"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

func run(ctx context.Context) error {
	// When CLI arguments are supplied, execute the one-off admin utility (such as
	// key generation) and exit. Running with no arguments starts the HTTP gateway server.
	if len(os.Args) > 1 {
		if os.Args[1] != "key-create" {
			return fmt.Errorf("unsupported command: use key-create or start without arguments")
		}

		return createKey(ctx, os.Args[2:], os.Stdout, os.Getenv)
	}

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("load gateway config: %w", err)
	}

	// Startup owns the pool. Its deferred close runs after HTTP draining completes.
	pool, err := store.OpenPool(ctx, cfg.databaseURL, store.PoolLimits{
		MaxConns:       cfg.databaseMaxConns,
		ConnectTimeout: cfg.databaseConnectTimeout,
	})
	if err != nil {
		return fmt.Errorf("initialize gateway database: %w", err)
	}
	defer pool.Close()

	keys := store.NewKeyStore(pool)
	authentication := auth.NewAuthenticator(keys, cfg.authLookupTimeout)

	// DefaultTransport is declared as the http.RoundTripper interface.
	// Assert its underlying type is *http.Transport and clone it so custom
	// timeouts and idle connection cleanup do not mutate the process-wide transport.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: cfg.upstreamConnectTimeout}).DialContext
	transport.TLSHandshakeTimeout = cfg.upstreamConnectTimeout

	transport.ResponseHeaderTimeout = cfg.upstreamHeaderTimeout
	defer transport.CloseIdleConnections()

	var (
		nonStreaming http.Handler
		streaming    http.Handler
		readiness    http.Handler
	)

	if cfg.upstreamURL != nil {
		nonStreaming = proxy.NewNonStreamingHandler(cfg.upstreamURL, transport, cfg.upstreamTimeout)
		streaming = proxy.NewStreamingHandler(cfg.upstreamURL, transport, cfg.streamIdleTimeout)
		readiness = backend.NewReadinessHandler(cfg.upstreamURL, transport, cfg.readinessTimeout)
	}

	routes := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming:   nonStreaming,
		Streaming:      streaming,
		Readiness:      readiness,
		Authentication: authentication,
		Limits: gateway.ChatLimits{
			MaxBodyBytes: cfg.maxBodyBytes,
			MaxTokens:    cfg.maxTokens,
		},
	})
	loggedRoutes := requestlog.NewHandler(routes, slog.Default())

	// Bound request reads and idle connections without limiting response streams.
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           loggedRoutes,
		ReadTimeout:       cfg.readTimeout,
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		IdleTimeout:       cfg.idleTimeout,
	}

	servers := []namedServer{{name: "gateway", server: server}}

	if cfg.adminEnabled {
		adminRoutes := admin.NewHandler(admin.HandlerOptions{
			Keys:          keys,
			Token:         cfg.adminToken,
			MaxBodyBytes:  cfg.adminMaxBodyBytes,
			CreateTimeout: cfg.adminCreateTimeout,
		})
		loggedAdminRoutes := requestlog.NewHandler(adminRoutes, slog.Default())
		adminServer := &http.Server{
			Addr:              cfg.adminAddr,
			Handler:           loggedAdminRoutes,
			ReadTimeout:       cfg.readTimeout,
			ReadHeaderTimeout: cfg.readHeaderTimeout,
			IdleTimeout:       cfg.idleTimeout,
			// Admin replies are small JSON; bound delivery as well as upload and insertion.
			WriteTimeout: cfg.readTimeout + cfg.adminCreateTimeout,
		}
		servers = append(servers, namedServer{name: "admin", server: adminServer})
	}

	bound, err := listenServers(servers)
	if err != nil {
		return err
	}

	return runServers(ctx, bound, cfg.shutdownTimeout)
}
