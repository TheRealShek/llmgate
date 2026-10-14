package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCreateKeyValidation rejects invalid options before generating a key or accessing PostgreSQL.
func TestCreateKeyValidation(t *testing.T) {
	for _, opts := range []CreateKeyOptions{
		{Name: "", RequestsPerMin: 1, TokensPerDay: 1},
		{Name: " \t", RequestsPerMin: 1, TokensPerDay: 1},
		{Name: strings.Repeat("a", 129), RequestsPerMin: 1, TokensPerDay: 1},
		{Name: "alice", RequestsPerMin: 0, TokensPerDay: 1},
		{Name: "alice", RequestsPerMin: -1, TokensPerDay: 1},
		{Name: "alice", RequestsPerMin: 1, TokensPerDay: 0},
		{Name: "alice", RequestsPerMin: 1, TokensPerDay: -1},
	} {
		key, secret, err := NewKeyStore(nil).CreateKey(t.Context(), opts)
		if !errors.Is(err, ErrInvalidKeyOptions) || key != (Key{}) || secret != "" {
			t.Fatal("invalid options must return ErrInvalidKeyOptions without a key")
		}
	}
}

// TestKeyLifecycle proves hash-only persistence, lookup through a new pool, and idempotent
// revocation.
func TestKeyLifecycle(t *testing.T) {
	pool := testPool(t)
	store := NewKeyStore(pool)
	ctx := t.Context()

	key, secret, err := store.CreateKey(ctx, CreateKeyOptions{
		Name:           " alice ",
		RequestsPerMin: 60,
		TokensPerDay:   10000,
	})
	if err != nil {
		t.Fatal(err)
	}

	random, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(secret, "lg_"))
	if err != nil || !strings.HasPrefix(secret, "lg_") || len(random) != 32 {
		t.Fatal("key must contain 32 random bytes in URL-safe encoding")
	}

	if key.ID <= 0 ||
		key.Name != "alice" ||
		key.RequestsPerMin != 60 ||
		key.TokensPerDay != 10000 ||
		key.CreatedAt.IsZero() {
		t.Fatal("stored identity and limits differ from the requested values")
	}

	var storedHash []byte

	if err := pool.QueryRow(
		ctx,
		"SELECT key_hash FROM api_keys WHERE id = $1",
		key.ID,
	).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}

	expected := sha256.Sum256([]byte(secret))
	if string(storedHash) != string(expected[:]) {
		t.Fatal("database must store the SHA-256 hash, not the raw key")
	}

	reopened, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	lookupStore := NewKeyStore(reopened)

	found, err := lookupStore.LookupKey(ctx, secret)

	if err != nil || found != key {
		t.Fatal("a new connection pool must find the persisted key")
	}

	for _, invalid := range []string{"", "unknown", secret + "x"} {
		if _, err := store.LookupKey(ctx, invalid); !errors.Is(err, ErrKeyNotFound) {
			t.Fatal("unknown key errors must wrap ErrKeyNotFound")
		}
	}

	if err := store.RevokeKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}

	var firstRevocation time.Time

	if err := pool.QueryRow(
		ctx,
		"SELECT revoked_at FROM api_keys WHERE id = $1",
		key.ID,
	).Scan(&firstRevocation); err != nil {
		t.Fatal(err)
	}

	if err := store.RevokeKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}

	var secondRevocation time.Time

	if err := pool.QueryRow(
		ctx,
		"SELECT revoked_at FROM api_keys WHERE id = $1",
		key.ID,
	).Scan(&secondRevocation); err != nil {
		t.Fatal(err)
	}

	if !firstRevocation.Equal(secondRevocation) {
		t.Fatal("repeated revocation must preserve the original timestamp")
	}

	if _, err := lookupStore.LookupKey(ctx, secret); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("a revoked key must no longer resolve")
	}

	if err := store.RevokeKey(ctx, key.ID+1); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("unknown key revocation errors must wrap ErrKeyNotFound")
	}
}

// TestKeyStoreCancellation preserves caller cancellation and never returns a secret on failed
// insertion.
func TestKeyStoreCancellation(t *testing.T) {
	store := NewKeyStore(testPool(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	key, secret, err := store.CreateKey(ctx, CreateKeyOptions{
		Name:           "alice",
		RequestsPerMin: 1,
		TokensPerDay:   1,
	})
	if !errors.Is(err, context.Canceled) || key != (Key{}) || secret != "" {
		t.Fatal("canceled insertion must return no key and preserve context.Canceled")
	}

	if _, err := store.LookupKey(ctx, "unknown"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled lookup must preserve context.Canceled")
	}

	if err := store.RevokeKey(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled revocation must preserve context.Canceled")
	}
}

// TestKeyStoreDatabaseFailure distinguishes unavailable storage from an unknown caller.
func TestKeyStoreDatabaseFailure(t *testing.T) {
	pool := testPool(t)
	pool.Close()

	store := NewKeyStore(pool)

	key, secret, err := store.CreateKey(t.Context(), CreateKeyOptions{
		Name:           "alice",
		RequestsPerMin: 1,
		TokensPerDay:   1,
	})

	if err == nil || errors.Is(err, ErrKeyNotFound) || key != (Key{}) || secret != "" {
		t.Fatal("unavailable storage must fail insertion without returning a key")
	}

	if _, err := store.LookupKey(t.Context(), "unknown"); err == nil ||
		errors.Is(err, ErrKeyNotFound) {
		t.Fatal("unavailable storage must not be mistaken for an unknown key")
	}

	if err := store.RevokeKey(t.Context(), 1); err == nil || errors.Is(err, ErrKeyNotFound) {
		t.Fatal("unavailable storage must fail revocation")
	}
}

// TestConcurrentKeys creates and revokes independent keys with a bounded connection pool.
func TestConcurrentKeys(t *testing.T) {
	pool := testPool(t)
	store := NewKeyStore(pool)

	var workers sync.WaitGroup

	ids := make(chan int64, 8)

	for range 8 {
		workers.Go(func() {
			key, secret, err := store.CreateKey(t.Context(), CreateKeyOptions{
				Name:           "concurrent",
				RequestsPerMin: 1,
				TokensPerDay:   1,
			})
			if err != nil {
				t.Error(err)
				return
			}

			ids <- key.ID

			if _, err := store.LookupKey(t.Context(), secret); err != nil {
				t.Error(err)
			}

			if err := store.RevokeKey(t.Context(), key.ID); err != nil {
				t.Error(err)
			}

			if _, err := store.LookupKey(t.Context(), secret); !errors.Is(err, ErrKeyNotFound) {
				t.Error("revoked key still resolves")
			}
		})
	}

	workers.Wait()
	close(ids)

	seen := make(map[int64]bool)
	for id := range ids {
		if seen[id] {
			t.Error("concurrent creations shared an ID")
		}

		seen[id] = true
	}

	if len(seen) != 8 || pool.Stat().TotalConns() > 2 {
		t.Fatal("all keys must be independent and the pool must stay bounded")
	}
}

// TestKeySchemaConstraints checks PostgreSQL rejects duplicate hashes and invalid values
// independently of Go.
func TestKeySchemaConstraints(t *testing.T) {
	pool := testPool(t)
	hash := sha256.Sum256([]byte("schema test key"))
	query := "INSERT INTO api_keys (key_hash, name, requests_per_min, tokens_per_day) " +
		"VALUES ($1, $2, $3, $4)"

	if _, err := pool.Exec(t.Context(), query, hash[:], "alice", 1, 1); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		hash     []byte
		name     string
		requests int64
		tokens   int64
	}{
		{hash: hash[:], name: "duplicate", requests: 1, tokens: 1},
		{hash: []byte("short"), name: "bad hash", requests: 1, tokens: 1},
		{hash: make([]byte, 32), name: "", requests: 1, tokens: 1},
		{hash: make([]byte, 32), name: strings.Repeat("a", 129), requests: 1, tokens: 1},
		{hash: make([]byte, 32), name: "bad rate", requests: 0, tokens: 1},
		{hash: make([]byte, 32), name: "bad quota", requests: 1, tokens: 0},
	} {
		if _, err := pool.Exec(t.Context(), query, tt.hash, tt.name, tt.requests, tt.tokens); err == nil {
			t.Error("database accepted invalid key data")
		}
	}
}

// TestKeyMigrationRollback checks the down migration removes the table and the up migration
// recreates it.
func TestKeyMigrationRollback(t *testing.T) {
	pool := testPool(t)

	down, err := os.ReadFile("../../migrations/000001_api_keys.down.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(t.Context(), string(down)); err != nil {
		t.Fatal(err)
	}

	var table *string

	if err := pool.QueryRow(
		t.Context(),
		"SELECT to_regclass('api_keys')::text",
	).Scan(&table); err != nil {
		t.Fatal(err)
	}

	if table != nil {
		t.Fatal("down migration left the key table behind")
	}

	up, err := os.ReadFile("../../migrations/000001_api_keys.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(t.Context(), string(up)); err != nil {
		t.Fatal(err)
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("LLMGATE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set LLMGATE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("invalid LLMGATE_TEST_DATABASE_URL")
	}

	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 2 * time.Second

	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	schema := "key_test_" + strings.ToLower(rand.Text())
	quotedSchema := pgx.Identifier{schema}.Sanitize()

	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close()
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		defer admin.Close()

		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})

	testCfg := cfg.Copy()
	testCfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, testCfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(pool.Close)

	migration, err := os.ReadFile("../../migrations/000001_api_keys.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}

	return pool
}
