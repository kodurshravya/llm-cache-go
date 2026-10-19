// Package cache stores responses in Redis and tracks template and model
// versions used for O(1) invalidation.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Entry is one cached upstream response. Body is always a complete JSON
// response (streams are assembled before they are stored).
type Entry struct {
	Status      int             `json:"status"`
	ContentType string          `json:"content_type"`
	Body        json.RawMessage `json:"body"`
	StoredAt    int64           `json:"stored_at"` // unix seconds
}

// Store wraps a Redis client.
type Store struct {
	rdb        *redis.Client
	defaultTTL time.Duration
	modelTTLs  map[string]time.Duration
}

// New returns a Store. modelTTLs may be nil.
func New(rdb *redis.Client, defaultTTL time.Duration, modelTTLs map[string]time.Duration) *Store {
	return &Store{rdb: rdb, defaultTTL: defaultTTL, modelTTLs: modelTTLs}
}

// TTL returns the TTL for a model, falling back to the default.
func (s *Store) TTL(model string) time.Duration {
	if d, ok := s.modelTTLs[model]; ok {
		return d
	}
	return s.defaultTTL
}

// Get returns the entry for key. found is false on a miss.
func (s *Store) Get(ctx context.Context, key string) (e *Entry, found bool, err error) {
	b, err := s.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var entry Entry
	if err := json.Unmarshal(b, &entry); err != nil {
		return nil, false, fmt.Errorf("cache: decode entry: %w", err)
	}
	return &entry, true, nil
}

// Set stores e under key with the model's TTL.
func (s *Store) Set(ctx context.Context, key, model string, e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, key, b, s.TTL(model)).Err()
}

func modelGenKey(model string) string { return "llmc:modelgen:" + model }
func tmplVerKey(name string) string   { return "llmc:tmplver:" + name }

// Versions returns the model generation and template version in one round
// trip. Missing counters are 0. template may be empty.
func (s *Store) Versions(ctx context.Context, model, template string) (modelGen, templateVer int64, err error) {
	ks := []string{modelGenKey(model)}
	if template != "" {
		ks = append(ks, tmplVerKey(template))
	}
	vals, err := s.rdb.MGet(ctx, ks...).Result()
	if err != nil {
		return 0, 0, err
	}
	if modelGen, err = toInt(vals[0]); err != nil {
		return 0, 0, err
	}
	if template != "" {
		if templateVer, err = toInt(vals[1]); err != nil {
			return 0, 0, err
		}
	}
	return modelGen, templateVer, nil
}

func toInt(v any) (int64, error) {
	if v == nil {
		return 0, nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("cache: unexpected counter type %T", v)
	}
	return strconv.ParseInt(s, 10, 64)
}

// BumpTemplate increments a template's version. Old entries are never read
// again and expire by TTL.
func (s *Store) BumpTemplate(ctx context.Context, name string) (int64, error) {
	return s.rdb.Incr(ctx, tmplVerKey(name)).Result()
}

// BumpModel increments a model's generation, flushing its namespace.
func (s *Store) BumpModel(ctx context.Context, model string) (int64, error) {
	return s.rdb.Incr(ctx, modelGenKey(model)).Result()
}

// Ping checks that Redis is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

// ParseModelTTLs parses "gpt-4.1=72h,text-embedding-3-small=720h".
func ParseModelTTLs(spec string) (map[string]time.Duration, error) {
	out := map[string]time.Duration{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		model, ttl, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("cache: bad MODEL_TTLS entry %q (want model=duration)", part)
		}
		d, err := time.ParseDuration(strings.TrimSpace(ttl))
		if err != nil {
			return nil, fmt.Errorf("cache: bad TTL for %q: %w", model, err)
		}
		out[strings.TrimSpace(model)] = d
	}
	return out, nil
}
