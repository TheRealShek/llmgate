package quota

import (
	"context"
	"crypto/rand"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const maxExact = int64(1<<53 - 1)

//go:embed reserve.lua
var reserveSource string

//go:embed settle.lua
var settleSource string

var (
	reserveScript = redis.NewScript(reserveSource)
	settleScript  = redis.NewScript(settleSource)
)

// Reservation identifies one allowance charged to its admission day.
// An empty ID means admission was denied.
type Reservation struct {
	ID           string
	KeyID        int64
	Day          int64
	PromptTokens int64
	OutputTokens int64
	RetryAfter   time.Duration
}

// Budget names the identity, daily ceilings, and requested token allowance.
type Budget struct {
	KeyID           int64
	Limit           int64
	HardLimit       int64
	PromptTokens    int64
	MaxOutputTokens int64
}

// RedisStore shares daily spent and reserved tokens through a caller-owned Redis client.
type RedisStore struct{ client *redis.Client }

// NewRedisStore keeps the existing client's deadlines, pool limits, and no-retry policy.
func NewRedisStore(client *redis.Client) *RedisStore { return &RedisStore{client: client} }

// Reserve atomically checks the normal limit and reserves input plus bounded output.
func (s *RedisStore) Reserve(ctx context.Context, budget Budget) (Reservation, error) {
	id, limit, hardLimit := budget.KeyID, budget.Limit, budget.HardLimit

	prompt, output := budget.PromptTokens, budget.MaxOutputTokens
	if id <= 0 || limit <= 0 || hardLimit < limit || hardLimit > maxExact ||
		prompt < 0 || prompt > maxExact || output <= 0 || output > maxExact {
		return Reservation{}, fmt.Errorf("validate token reservation: %w", ErrInvalidLimit)
	}

	if s == nil || s.client == nil {
		return Reservation{}, fmt.Errorf("reserve daily tokens: %w", ErrUnavailable)
	}

	token := rand.Text()

	values, err := reserveScript.Run(ctx, s.client, []string{quotaKey(id)}, limit, hardLimit, prompt, output, token).Int64Slice()
	if err != nil {
		return Reservation{}, fmt.Errorf("reserve daily tokens: %w", ErrUnavailable)
	}

	if len(values) != 4 || values[1] < 0 || (values[0] != 0 && values[0] != 1) ||
		(values[0] == 1 && (values[2] <= 0 || values[2] > output || values[3] != 0)) ||
		(values[0] == 0 && (values[2] != 0 || values[3] <= 0 || values[3] > 86400)) {
		return Reservation{}, fmt.Errorf("decode token reservation: %w", ErrUnavailable)
	}

	result := Reservation{
		KeyID: id, Day: values[1], PromptTokens: prompt, OutputTokens: values[2],
		RetryAfter: time.Duration(values[3]) * time.Second,
	}
	if values[0] == 1 {
		result.ID = token
	}

	return result, nil
}

// Settle replaces a pending allowance once; negative charge retains its full amount.
// Missing or expired reservation IDs are harmless, including requests crossing midnight.
func (s *RedisStore) Settle(ctx context.Context, r Reservation, charge int64) error {
	if r.KeyID <= 0 || r.ID == "" || r.Day < 0 || charge < -1 || charge > maxExact {
		return fmt.Errorf("validate token settlement: %w", ErrInvalidLimit)
	}

	if s == nil || s.client == nil {
		return fmt.Errorf("settle daily tokens: %w", ErrUnavailable)
	}

	result, err := settleScript.Run(ctx, s.client, []string{quotaKey(r.KeyID)}, r.Day, r.ID, charge).Int64()
	if err != nil || (result != 0 && result != 1) {
		return fmt.Errorf("settle daily tokens: %w", ErrUnavailable)
	}

	return nil
}

func quotaKey(id int64) string { return "llmgate:tokens:v1:" + strconv.FormatInt(id, 10) }
