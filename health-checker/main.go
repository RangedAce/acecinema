package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"acecinema/pkg/db"
	"acecinema/pkg/logger"
)

type backend struct {
	ID         string
	Host       string
	Port       int
	HealthPath string
}

type config struct {
	DBURL      string
	Interval   time.Duration
	Timeout    time.Duration
	Concurrent int
}

func loadConfig() (config, error) {
	interval, _ := time.ParseDuration(getenv("CHECK_INTERVAL", "10s"))
	timeout, _ := time.ParseDuration(getenv("CHECK_TIMEOUT", "2s"))
	concurrency := atoiDefault(os.Getenv("CHECK_CONCURRENCY"), 8)
	return config{
		DBURL:      os.Getenv("DB_URL"),
		Interval:   interval,
		Timeout:    timeout,
		Concurrent: concurrency,
	}, nil
}

func main() {
	log := logger.New()
	cfg, _ := loadConfig()
	if cfg.DBURL == "" {
		log.Fatal().Msg("DB_URL required")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DBURL)
	if err != nil {
		log.Fatal().Err(err).Msg("db connect failed")
	}
	defer pool.Close()

	client := &http.Client{Timeout: cfg.Timeout}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		runChecks(ctx, pool, client, cfg.Concurrent, log)
		<-ticker.C
	}
}

func runChecks(ctx context.Context, pool *pgxpool.Pool, client *http.Client, concurrent int, log zerolog.Logger) {
	backends, err := loadBackends(ctx, pool)
	if err != nil {
		log.Error().Err(err).Msg("load backends")
		return
	}
	sem := make(chan struct{}, concurrent)
	var wg sync.WaitGroup
	for _, b := range backends {
		wg.Add(1)
		sem <- struct{}{}
		go func(b backend) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, latency := probe(client, b)
			if err := updateHealth(ctx, pool, b.ID, ok, latency); err != nil {
				log.Error().Err(err).Str("backend", b.ID).Msg("update health")
			}
		}(b)
	}
	wg.Wait()
}

func loadBackends(ctx context.Context, pool *pgxpool.Pool) ([]backend, error) {
	rows, err := pool.Query(ctx, "SELECT id,host,port,health_path FROM backends")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []backend
	for rows.Next() {
		var b backend
		if err := rows.Scan(&b.ID, &b.Host, &b.Port, &b.HealthPath); err != nil {
			return nil, err
		}
		res = append(res, b)
	}
	return res, rows.Err()
}

func probe(client *http.Client, b backend) (bool, int) {
	start := time.Now()
	url := "http://" + b.Host + ":" + itoa(b.Port) + b.HealthPath
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, int(time.Since(start) / time.Millisecond)
	}
	return false, int(time.Since(start) / time.Millisecond)
}

func updateHealth(ctx context.Context, pool *pgxpool.Pool, backendID string, ok bool, latency int) error {
	status := "down"
	if ok {
		status = "up"
	}
	now := time.Now()
	_, err := pool.Exec(ctx, `
		UPDATE backends SET is_healthy=$1,last_checked=$2 WHERE id=$3;
		INSERT INTO health_events (backend_id,status,latency_ms,at) VALUES ($3,$4,$5,$2);
	`, ok, now, backendID, status, latency)
	return err
}

// helpers (tiny to avoid extra deps)
func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiDefault(v string, def int) int {
	if v == "" {
		return def
	}
	var out int
	_, err := fmt.Sscanf(v, "%d", &out)
	if err != nil {
		return def
	}
	return out
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}
