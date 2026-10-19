// Command llmcache runs the caching proxy.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kodurshravya/llm-cache-go/internal/cache"
	"github.com/kodurshravya/llm-cache-go/internal/metrics"
	"github.com/kodurshravya/llm-cache-go/internal/proxy"
)

type config struct {
	listen          string
	upstream        *url.URL
	redisURL        string
	adminToken      string
	logLevel        slog.Level
	defaultTTL      time.Duration
	upstreamTimeout time.Duration
	modelTTLs       map[string]time.Duration
	maxEntryBytes   int
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{
		listen:     env("LISTEN_ADDR", ":8080"),
		redisURL:   env("REDIS_URL", "redis://localhost:6379/0"),
		adminToken: os.Getenv("ADMIN_TOKEN"),
	}
	var err error
	if c.upstream, err = url.Parse(env("UPSTREAM_URL", "https://api.openai.com")); err != nil || c.upstream.Host == "" {
		return c, fmt.Errorf("UPSTREAM_URL must be an absolute URL")
	}
	if err = c.logLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	if c.defaultTTL, err = time.ParseDuration(env("DEFAULT_TTL", "24h")); err != nil {
		return c, fmt.Errorf("DEFAULT_TTL: %w", err)
	}
	if c.upstreamTimeout, err = time.ParseDuration(env("UPSTREAM_TIMEOUT", "60s")); err != nil {
		return c, fmt.Errorf("UPSTREAM_TIMEOUT: %w", err)
	}
	if c.modelTTLs, err = cache.ParseModelTTLs(env("MODEL_TTLS", "gpt-4.1=72h,text-embedding-3-small=720h")); err != nil {
		return c, fmt.Errorf("MODEL_TTLS: %w", err)
	}
	if c.maxEntryBytes, err = strconv.Atoi(env("MAX_ENTRY_BYTES", "1048576")); err != nil {
		return c, fmt.Errorf("MAX_ENTRY_BYTES: %w", err)
	}
	return c, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))

	opt, err := redis.ParseURL(cfg.redisURL)
	if err != nil {
		return fmt.Errorf("REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	srv := proxy.New(proxy.Config{
		UpstreamURL:     cfg.upstream,
		UpstreamTimeout: cfg.upstreamTimeout,
		MaxEntryBytes:   cfg.maxEntryBytes,
		AdminToken:      cfg.adminToken,
	}, cache.New(rdb, cfg.defaultTTL, cfg.modelTTLs), metrics.New(), log)

	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.listen, "upstream", cfg.upstream.String(), "admin_enabled", cfg.adminToken != "")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down: draining in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.upstreamTimeout+5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
