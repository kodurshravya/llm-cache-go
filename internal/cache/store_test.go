package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, time.Hour, map[string]time.Duration{"emb": 30 * 24 * time.Hour}), mr
}

func TestSetGetAndTTL(t *testing.T) {
	ctx := context.Background()
	s, mr := newStore(t)

	if _, found, err := s.Get(ctx, "k"); err != nil || found {
		t.Fatalf("empty cache: found=%v err=%v", found, err)
	}
	e := &Entry{Status: 200, ContentType: "application/json", Body: []byte(`{"a":1}`), StoredAt: 123}
	if err := s.Set(ctx, "k", "emb", e); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Get(ctx, "k")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if string(got.Body) != `{"a":1}` || got.StoredAt != 123 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if ttl := mr.TTL("k"); ttl != 30*24*time.Hour {
		t.Fatalf("model TTL not applied: %v", ttl)
	}
	if s.TTL("other") != time.Hour {
		t.Fatal("default TTL not applied")
	}
}

func TestVersions(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	gen, ver, err := s.Versions(ctx, "m", "tmpl")
	if err != nil || gen != 0 || ver != 0 {
		t.Fatalf("defaults: gen=%d ver=%d err=%v", gen, ver, err)
	}
	if _, err := s.BumpTemplate(ctx, "tmpl"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BumpModel(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BumpModel(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	gen, ver, err = s.Versions(ctx, "m", "tmpl")
	if err != nil || gen != 2 || ver != 1 {
		t.Fatalf("after bumps: gen=%d ver=%d err=%v", gen, ver, err)
	}
	if _, ver, _ = s.Versions(ctx, "m", ""); ver != 0 {
		t.Fatal("no template should mean version 0")
	}
}

func TestParseModelTTLs(t *testing.T) {
	got, err := ParseModelTTLs("gpt-4.1=72h, text-embedding-3-small=720h,")
	if err != nil {
		t.Fatal(err)
	}
	if got["gpt-4.1"] != 72*time.Hour || got["text-embedding-3-small"] != 720*time.Hour {
		t.Fatalf("got %v", got)
	}
	for _, bad := range []string{"gpt-4.1", "=1h", "m=forever"} {
		if _, err := ParseModelTTLs(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
