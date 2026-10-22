// Package store persists gateway keys and inference usage in PostgreSQL.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Key contains caller identity and limits, never the secret or its hash.
type Key struct {
	ID             int64
	Name           string
	RequestsPerMin int64
	TokensPerDay   int64
	CreatedAt      time.Time
}

// CreateKeyParams contains the label and limits for a new API key.
type CreateKeyParams struct {
	Name           string
	RequestsPerMin int64
	TokensPerDay   int64
}

// KeyStore creates, looks up, and revokes keys using a caller-owned pool.
type KeyStore struct {
	pool *pgxpool.Pool
}

// NewKeyStore uses pool without taking ownership of its lifetime or connection limits.
func NewKeyStore(pool *pgxpool.Pool) *KeyStore {
	return &KeyStore{pool: pool}
}

// CreateKey persists a hash and returns the raw key only after insertion succeeds.
// The caller must deliver the secret once and must not log it.
func (s *KeyStore) CreateKey(ctx context.Context, params CreateKeyParams) (Key, string, error) {
	name := strings.TrimSpace(params.Name)
	if name == "" || len(name) > 128 || params.RequestsPerMin <= 0 || params.TokensPerDay <= 0 {
		return Key{}, "", ErrInvalidKeyOptions
	}

	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Key{}, "", fmt.Errorf("generate API key: %w", err)
	}

	secret := "lg_" + base64.RawURLEncoding.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(secret))

	var key Key

	err := s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (key_hash, name, requests_per_min, tokens_per_day)
		VALUES ($1, $2, $3, $4)
		RETURNING id, name, requests_per_min, tokens_per_day, created_at`,
		hash[:], name, params.RequestsPerMin, params.TokensPerDay,
	).Scan(&key.ID, &key.Name, &key.RequestsPerMin, &key.TokensPerDay, &key.CreatedAt)
	if err != nil {
		return Key{}, "", fmt.Errorf("create API key: %w", err)
	}

	return key, secret, nil
}

// LookupKey hashes the caller's secret and returns only an active key's identity and limits.
// Unknown or revoked keys return ErrKeyNotFound; database failures retain their original cause.
func (s *KeyStore) LookupKey(ctx context.Context, secret string) (Key, error) {
	if secret == "" {
		return Key{}, ErrKeyNotFound
	}

	hash := sha256.Sum256([]byte(secret))

	var key Key

	err := s.pool.QueryRow(ctx, `
		SELECT id, name, requests_per_min, tokens_per_day, created_at
		FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, hash[:],
	).Scan(&key.ID, &key.Name, &key.RequestsPerMin, &key.TokensPerDay, &key.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrKeyNotFound
	}

	if err != nil {
		return Key{}, fmt.Errorf("look up API key: %w", err)
	}

	return key, nil
}

// RevokeKey disables lookup while preserving the record and its first revocation time.
// Revoking an existing key twice succeeds; an unknown ID returns ErrKeyNotFound.
func (s *KeyStore) RevokeKey(ctx context.Context, id int64) error {
	result, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("revoke API key %d: %w", id, err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("revoke API key %d: %w", id, ErrKeyNotFound)
	}

	return nil
}
