package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/therealshek/llmgate/internal/accounting"
	"github.com/therealshek/llmgate/internal/admin"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
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
	authentication := auth.NewAuthenticator(keys, auth.Options{
		LookupTimeout: cfg.authLookupTimeout,
		CacheTTL:      cfg.authCacheTTL,
		CacheCapacity: cfg.authCacheCapacity,
	})

	// Startup owns this pool too. Closing it after HTTP draining lets admitted
	// requests finish without losing shared dependencies during shutdown.
	redisClient, err := ratelimit.OpenClient(ctx, cfg.redisURL, ratelimit.ClientLimits{
		MaxConns:       cfg.redisMaxConns,
		ConnectTimeout: cfg.redisConnectTimeout,
		CommandTimeout: max(cfg.rateLimitTimeout, cfg.quotaRedisTimeout, cfg.usageWriteTimeout),
	})
	if err != nil {
		return fmt.Errorf("initialize request limiting: %w", err)
	}
	defer redisClient.Close()

	buckets := ratelimit.NewRedisStore(redisClient)
	requestLimits := ratelimit.NewLimiter(buckets, cfg.rateLimitTimeout)

	usageStore := store.NewUsageStore(pool)

	accountingQueue, err := accounting.Open(ctx, redisClient, usageStore, accounting.Options{
		Namespace:     "llmgate:accounting:v1",
		MaxPending:    cfg.accountingMaxPending,
		BatchSize:     cfg.accountingBatchSize,
		FlushInterval: cfg.accountingFlushInterval,
		Timeout:       cfg.usageWriteTimeout,
		LeaseTimeout:  cfg.accountingLeaseTimeout,
		MaxAOFBytes:   cfg.accountingMaxAOFBytes,
		Retention:     cfg.accountingRetention,
		PruneInterval: cfg.accountingPruneInterval,
	})
	if err != nil {
		return fmt.Errorf("initialize durable accounting: %w", err)
	}

	// Keep delivery running through HTTP drain; join it before closing either pool.
	accountingCtx, stopAccounting := context.WithCancel(context.Background())

	accountingDone := make(chan struct{})
	go func() {
		defer close(accountingDone)

		accountingQueue.Run(accountingCtx)
	}()

	defer func() {
		stopAccounting()
		<-accountingDone
	}()

	usageRecorder := usage.NewRecorder(usage.Options{
		Store:        accountingQueue,
		Backend:      "primary",
		WriteTimeout: cfg.usageWriteTimeout,
		MaxBytes:     cfg.usageMaxBytes,
	})

	// DefaultTransport is declared as the http.RoundTripper interface.
	// Assert its underlying type is *http.Transport and clone it so custom
	// timeouts and idle connection cleanup do not mutate the process-wide transport.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: cfg.upstreamConnectTimeout}).DialContext
	transport.TLSHandshakeTimeout = cfg.upstreamConnectTimeout

	transport.ResponseHeaderTimeout = cfg.upstreamHeaderTimeout
	defer transport.CloseIdleConnections()

	promptCounter := quota.NewCounter(cfg.upstreamURL, transport, cfg.quotaCountMaxBytes)
	tokenQuotas := quota.NewLimiter(promptCounter, quota.NewRedisStore(redisClient), quota.Options{
		CountTimeout: cfg.quotaCountTimeout, RedisTimeout: cfg.quotaRedisTimeout, GracePercent: cfg.quotaGracePercent,
	})

	var (
		nonStreaming http.Handler
		streaming    http.Handler
		readiness    http.Handler
	)

	if cfg.upstreamURL != nil {
		nonStreaming = proxy.NewNonStreamingHandler(cfg.upstreamURL, transport, cfg.upstreamTimeout)
		streaming = proxy.NewStreamingHandler(cfg.upstreamURL, transport, cfg.streamIdleTimeout)

		registry, err := backend.NewRegistry(
			[]backend.Target{{Name: "primary", URL: cfg.upstreamURL}},
			transport,
			backend.HealthOptions{
				Interval: cfg.backendHealthInterval, Timeout: cfg.readinessTimeout,
				Failures: cfg.backendHealthFailures, Successes: cfg.backendHealthSuccesses,
			},
		)
		if err != nil {
			return fmt.Errorf("initialize backend registry: %w", err)
		}

		// Health monitoring survives HTTP drain, then stops before transport cleanup.
		healthCtx, stopHealth := context.WithCancel(context.Background())

		healthDone := make(chan struct{})
		go func() {
			defer close(healthDone)

			registry.Run(healthCtx)
		}()

		defer func() {
			stopHealth()
			<-healthDone
		}()

		readiness = backend.NewReadinessHandler(registry)
	}

	gatewayMetrics := metrics.New(metrics.Options{
		KeyCapacity: cfg.metricsKeyCapacity, MaxScrapes: cfg.metricsMaxScrapes, ScrapeTimeout: cfg.metricsScrapeTimeout,
	})
	routes := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming:   nonStreaming,
		Streaming:      streaming,
		Readiness:      readiness,
		Authentication: authentication,
		RequestLimits:  requestLimits,
		Usage:          usageRecorder,
		Quota:          tokenQuotas,
		Metrics:        gatewayMetrics,
		Limits: gateway.ChatLimits{
			MaxBodyBytes: cfg.maxBodyBytes,
			MaxTokens:    cfg.maxTokens,
		},
	})
	observedRoutes := gatewayMetrics.Observe(accountingQueue.Protect(routes))
	loggedRoutes := requestlog.NewHandler(observedRoutes, slog.Default())

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
			Revoker:       authentication,
			Token:         cfg.adminToken,
			MaxBodyBytes:  cfg.adminMaxBodyBytes,
			CreateTimeout: cfg.adminCreateTimeout,
			RevokeTimeout: cfg.adminRevokeTimeout,
		})
		loggedAdminRoutes := requestlog.NewHandler(adminRoutes, slog.Default())
		adminServer := &http.Server{
			Addr:              cfg.adminAddr,
			Handler:           loggedAdminRoutes,
			ReadTimeout:       cfg.readTimeout,
			ReadHeaderTimeout: cfg.readHeaderTimeout,
			IdleTimeout:       cfg.idleTimeout,
			// Bound admin uploads, storage operations, and response delivery.
			WriteTimeout: cfg.readTimeout + max(cfg.adminCreateTimeout, cfg.adminRevokeTimeout),
		}
		servers = append(servers, namedServer{name: "admin", server: adminServer})
	}

	bound, err := listenServers(servers)
	if err != nil {
		return err
	}

	return runServers(ctx, bound, cfg.shutdownTimeout, cfg.shutdownCleanupTimeout)
}
