package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/therealshek/llmgate/internal/store"
)

// createKey provisions a local key through the same store used by authentication.
// It writes the secret once after insertion succeeds; diagnostics never contain it.
func createKey(ctx context.Context, args []string, output io.Writer, getenv func(string) string) error {
	flags := flag.NewFlagSet("key-create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var params store.CreateKeyParams

	flags.StringVar(&params.Name, "name", "", "key label, required")
	flags.Int64Var(&params.RequestsPerMin, "requests-per-min", 60, "stored request limit")
	flags.Int64Var(&params.TokensPerDay, "tokens-per-day", 100000, "stored daily token limit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage := "Usage: gateway key-create -name LABEL " +
				"[-requests-per-min 60] [-tokens-per-day 100000]"
			_, err := fmt.Fprintln(output, usage)

			return err
		}

		return fmt.Errorf("parse key-create options: invalid flags: %w", &keyCreateError{cause: err})
	}

	if len(flags.Args()) != 0 {
		return fmt.Errorf("parse key-create options: unexpected arguments")
	}

	params.Name = strings.TrimSpace(params.Name)
	if params.Name == "" || len(params.Name) > 128 || params.RequestsPerMin <= 0 || params.TokensPerDay <= 0 {
		return fmt.Errorf("validate key-create options: %w", store.ErrInvalidKeyOptions)
	}

	cfg, err := loadConfig(getenv)
	if err != nil {
		return fmt.Errorf("load key-create config: %w", err)
	}

	pool, err := store.OpenPool(ctx, cfg.postgres.url, store.PoolLimits{
		MaxConns:       cfg.postgres.maxConnections,
		ConnectTimeout: cfg.postgres.connectTimeout,
	})
	if err != nil {
		return fmt.Errorf("initialize key-create database: %w", err)
	}
	defer pool.Close()

	operationCtx, cancel := context.WithTimeout(ctx, cfg.auth.keyStoreTimeout)
	defer cancel()

	key, secret, err := store.NewKeyStore(pool).CreateKey(operationCtx, params)
	if err != nil {
		// Database diagnostics may contain credentials. Keep the failure cause available
		// through Unwrap while exposing only a fixed message at the command boundary.
		return fmt.Errorf("create local API key: %w", &keyCreateError{cause: err})
	}

	result := struct {
		ID             int64  `json:"id"`
		Name           string `json:"name"`
		APIKey         string `json:"api_key"`
		RequestsPerMin int64  `json:"requests_per_min"`
		TokensPerDay   int64  `json:"tokens_per_day"`
	}{
		ID:             key.ID,
		Name:           key.Name,
		APIKey:         secret,
		RequestsPerMin: key.RequestsPerMin,
		TokensPerDay:   key.TokensPerDay,
	}

	if err := json.NewEncoder(output).Encode(result); err != nil {
		return fmt.Errorf(
			"write created key: key was stored but its secret could not be delivered: %w",
			&keyCreateError{cause: err},
		)
	}

	return nil
}
