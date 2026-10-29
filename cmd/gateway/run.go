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
	"github.com/therealshek/llmgate/internal/queue"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/router"
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
	pool, err := store.OpenPool(ctx, cfg.postgres.url, store.PoolLimits{
		MaxConns:       cfg.postgres.maxConnections,
		ConnectTimeout: cfg.postgres.connectTimeout,
	})
	if err != nil {
		return fmt.Errorf("initialize gateway database: %w", err)
	}
	defer pool.Close()

	keys := store.NewKeyStore(pool)
	authentication := auth.NewAuthenticator(keys, auth.Options{
		LookupTimeout: cfg.auth.keyStoreTimeout,
		CacheTTL:      cfg.auth.cacheTTL,
		CacheCapacity: cfg.auth.cacheCapacity,
	})

	// Startup owns this pool too. Closing it after HTTP draining lets admitted
	// requests finish without losing shared dependencies during shutdown.
	redisClient, err := ratelimit.OpenClient(ctx, cfg.redis.url, ratelimit.ClientLimits{
		MaxConns:       cfg.redis.maxConnections,
		ConnectTimeout: cfg.redis.connectTimeout,
		CommandTimeout: max(cfg.rateLimit.checkTimeout, cfg.quota.redisTimeout, cfg.accounting.writeTimeout),
	})
	if err != nil {
		return fmt.Errorf("initialize request limiting: %w", err)
	}
	defer redisClient.Close()

	buckets := ratelimit.NewRedisStore(redisClient)
	requestLimits := ratelimit.NewLimiter(buckets, cfg.rateLimit.checkTimeout)

	usageStore := store.NewUsageStore(pool)

	accountingQueue, err := accounting.Open(ctx, redisClient, usageStore, accounting.Options{
		Namespace:     "llmgate:accounting:v1",
		MaxPending:    cfg.accounting.maxPendingAttempts,
		BatchSize:     cfg.accounting.batchSize,
		FlushInterval: cfg.accounting.flushInterval,
		Timeout:       cfg.accounting.writeTimeout,
		LeaseTimeout:  cfg.accounting.ownershipLeaseTimeout,
		MaxAOFBytes:   cfg.accounting.maxAOFBytes,
		Retention:     cfg.accounting.retentionPeriod,
		PruneInterval: cfg.accounting.pruneInterval,
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

	// DefaultTransport is declared as the http.RoundTripper interface.
	// Assert its underlying type is *http.Transport and clone it so custom
	// timeouts and idle connection cleanup do not mutate the process-wide transport.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: cfg.inference.connectTimeout}).DialContext
	transport.TLSHandshakeTimeout = cfg.inference.connectTimeout

	transport.ResponseHeaderTimeout = cfg.inference.responseHeaderTimeout
	defer transport.CloseIdleConnections()

	// Each backend gets its own queue, counter, proxies, and accounting identity.
	// Redis quota state and durable accounting storage remain shared across targets.
	targets := make([]backend.Target, 0, len(cfg.inference.targets))
	backends := make([]gateway.Backend, 0, len(cfg.inference.targets))

	admissions := make(map[string]*queue.Queue, len(cfg.inference.targets))
	for _, target := range cfg.inference.targets {
		admission, err := queue.New(queue.Options{
			Slots: target.slots, Capacity: target.queueCapacity, WaitTimeout: cfg.inference.admission.queueWaitTimeout,
		})
		if err != nil {
			return fmt.Errorf("initialize backend %s admission: %w", target.name, err)
		}

		counter := quota.NewCounter(target.url, transport, cfg.quota.maxCountResponseBytes)
		tokenQuota := quota.NewLimiter(counter, quota.NewRedisStore(redisClient), quota.Options{
			CountTimeout: cfg.quota.promptCountTimeout, RedisTimeout: cfg.quota.redisTimeout, GracePercent: cfg.quota.gracePercent,
		})
		recorder := usage.NewRecorder(usage.Options{
			Store: accountingQueue, Backend: target.name, WriteTimeout: cfg.accounting.writeTimeout, MaxBytes: cfg.accounting.observationMaxBytes,
		})
		targets = append(targets, backend.Target{Name: target.name, URL: target.url})
		admissions[target.name] = admission
		backends = append(backends, gateway.Backend{
			Name: target.name, Admission: admission, Quota: tokenQuota, Usage: recorder,
			NonStreaming: proxy.NewNonStreamingHandler(target.url, transport, cfg.inference.jsonResponseTimeout),
			Streaming:    proxy.NewStreamingHandler(target.url, transport, cfg.inference.streamIdleTimeout),
		})
	}

	registry, err := backend.NewRegistry(targets, transport, backend.HealthOptions{
		Interval: cfg.inference.health.probeInterval, Timeout: cfg.inference.health.probeTimeout,
		Failures: cfg.inference.health.failureThreshold, Successes: cfg.inference.health.successThreshold,
	})
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

	gatewayMetrics := metrics.New(metrics.Options{
		KeyCapacity: cfg.metrics.keyLabelCapacity, MaxScrapes: cfg.metrics.maxConcurrentScrapes, ScrapeTimeout: cfg.metrics.scrapeTimeout,
		Backends: admissions,
	})
	routes := gateway.NewHandler(gateway.HandlerOptions{
		CircuitBreaker: router.CircuitOptions{
			FailureThreshold: cfg.inference.circuitBreaker.failureThreshold,
			Cooldown:         cfg.inference.circuitBreaker.cooldown,
		},
		Backends: backends, Health: registry, Readiness: backend.NewReadinessHandler(registry),
		Authentication: authentication, RequestLimits: requestLimits, Metrics: gatewayMetrics,
		Limits: gateway.ChatLimits{
			MaxBodyBytes:       cfg.chat.maxRequestBodyBytes,
			MaxTokens:          cfg.chat.maxOutputTokens,
			MaxAttempts:        cfg.chat.maxBackendAttempts,
			StreamErrorTimeout: cfg.inference.streamIdleTimeout,
		},
	})
	observedRoutes := gatewayMetrics.Observe(accountingQueue.Protect(routes))
	loggedRoutes := requestlog.NewHandler(observedRoutes, slog.Default())

	// Bound request reads and idle connections without limiting response streams.
	server := &http.Server{
		Addr:              cfg.server.address,
		Handler:           loggedRoutes,
		ReadTimeout:       cfg.server.readTimeout,
		ReadHeaderTimeout: cfg.server.readHeaderTimeout,
		IdleTimeout:       cfg.server.idleTimeout,
	}

	servers := []namedServer{{name: "gateway", server: server}}

	if cfg.admin.enabled {
		adminRoutes := admin.NewHandler(admin.HandlerOptions{
			Keys:          keys,
			Revoker:       authentication,
			Token:         cfg.admin.token,
			MaxBodyBytes:  cfg.admin.maxRequestBodyBytes,
			CreateTimeout: cfg.admin.createTimeout,
			RevokeTimeout: cfg.admin.revokeTimeout,
		})
		loggedAdminRoutes := requestlog.NewHandler(adminRoutes, slog.Default())
		adminServer := &http.Server{
			Addr:              cfg.admin.address,
			Handler:           loggedAdminRoutes,
			ReadTimeout:       cfg.server.readTimeout,
			ReadHeaderTimeout: cfg.server.readHeaderTimeout,
			IdleTimeout:       cfg.server.idleTimeout,
			// Bound admin uploads, storage operations, and response delivery.
			WriteTimeout: cfg.server.readTimeout + max(cfg.admin.createTimeout, cfg.admin.revokeTimeout),
		}
		servers = append(servers, namedServer{name: "admin", server: adminServer})
	}

	bound, err := listenServers(servers)
	if err != nil {
		return err
	}

	return runServers(ctx, bound, cfg.server.shutdownTimeout, cfg.server.shutdownCleanupTimeout)
}
