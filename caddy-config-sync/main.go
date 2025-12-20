package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"acecinema/pkg/db"
	"acecinema/pkg/logger"
)

type service struct {
	Name     string
	Domains  []string
	Backends []backend
}

type backend struct {
	Host      string
	Port      int
	Weight    int
	HealthURI string
}

type config struct {
	DBURL        string
	Caddyfile    string
	AdminURL     string
	DeployMode   string
	SSHHost      string
	SSHUser      string
	SSHPath      string
	DryRun       bool
	Interval     time.Duration
}

func loadConfig() (config, error) {
	dry := strings.ToLower(os.Getenv("DRY_RUN")) == "true"
	interval, _ := time.ParseDuration(getenv("SYNC_INTERVAL", "30s"))
	return config{
		DBURL:      os.Getenv("DB_URL"),
		Caddyfile:  getenv("CADDYFILE_PATH", "/out/Caddyfile"),
		AdminURL:   os.Getenv("CADDY_ADMIN_URL"),
		DeployMode: getenv("CADDY_DEPLOY_MODE", "local"),
		SSHHost:    os.Getenv("CADDY_SSH_HOST"),
		SSHUser:    os.Getenv("CADDY_SSH_USER"),
		SSHPath:    getenv("CADDY_SSH_PATH", "/etc/caddy/Caddyfile"),
		DryRun:     dry,
		Interval:   interval,
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

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		if err := syncOnce(ctx, pool, cfg, log); err != nil {
			log.Error().Err(err).Msg("sync failed")
		}
		<-ticker.C
	}
}

func syncOnce(ctx context.Context, pool *pgxpool.Pool, cfg config, log zerolog.Logger) error {
	services, err := loadServices(ctx, pool)
	if err != nil {
		return err
	}
	body := renderCaddyfile(services)
	if cfg.DryRun {
		log.Info().Msg("dry-run enabled, not applying config")
		fmt.Println(body)
		return nil
	}
	if err := validateCaddy(body); err != nil {
		return fmt.Errorf("caddy validation failed: %w", err)
	}
	if err := writeTarget(cfg, body); err != nil {
		return fmt.Errorf("write target: %w", err)
	}
	if cfg.AdminURL != "" {
		if err := reloadCaddy(cfg.AdminURL, body); err != nil {
			return fmt.Errorf("reload failed: %w", err)
		}
	}
	log.Info().Int("services", len(services)).Msg("caddy config applied")
	return nil
}

func loadServices(ctx context.Context, pool *pgxpool.Pool) ([]service, error) {
	rows, err := pool.Query(ctx, `
		SELECT s.name, s.domains, b.host, b.port, b.weight, b.health_path
		FROM services s
		JOIN backends b ON b.service_id = s.id
		WHERE b.is_healthy = true
		ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	svcMap := map[string]*service{}
	for rows.Next() {
		var name string
		var domains []string
		var b backend
		if err := rows.Scan(&name, &domains, &b.Host, &b.Port, &b.Weight, &b.HealthURI); err != nil {
			return nil, err
		}
		svc, ok := svcMap[name]
		if !ok {
			svc = &service{Name: name, Domains: domains}
			svcMap[name] = svc
		}
		svc.Backends = append(svc.Backends, b)
	}
	var out []service
	for _, s := range svcMap {
		out = append(out, *s)
	}
	return out, rows.Err()
}

func renderCaddyfile(services []service) string {
	var b strings.Builder
	for _, s := range services {
		for _, host := range s.Domains {
			b.WriteString(host)
			b.WriteString(" {\n")
			b.WriteString("  encode gzip\n")
			b.WriteString("  log\n")
			b.WriteString("  handle {\n")
			b.WriteString("    reverse_proxy {\n")
			b.WriteString("      lb_policy random_choose\n")
			b.WriteString("      lb_try_duration 30s\n")
			b.WriteString("      lb_try_interval 2s\n")
			b.WriteString("      health_interval 10s\n")
			b.WriteString("      health_timeout 2s\n")
			if len(s.Backends) > 0 && s.Backends[0].HealthURI != "" {
				b.WriteString("      health_uri " + s.Backends[0].HealthURI + "\n")
			}
			for _, up := range s.Backends {
				b.WriteString(fmt.Sprintf("      upstream %s:%d {\n", up.Host, up.Port))
				b.WriteString(fmt.Sprintf("        lb_weight %d\n", up.Weight))
				b.WriteString("        max_fails 2\n")
				b.WriteString("        fail_duration 15s\n")
				b.WriteString("      }\n")
			}
			b.WriteString("    }\n")
			b.WriteString("  }\n")
			b.WriteString("}\n\n")
		}
	}
	return b.String()
}

func validateCaddy(body string) error {
	cmd := exec.Command("caddy", "fmt", "--validate")
	cmd.Stdin = strings.NewReader(body)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func writeTarget(cfg config, body string) error {
	if cfg.DeployMode == "ssh" {
		// Placeholder: implement rsync/scp if needed.
		return fmt.Errorf("ssh deploy not implemented in skeleton")
	}
	return os.WriteFile(cfg.Caddyfile, []byte(body), 0644)
}

func reloadCaddy(adminURL string, body string) error {
	req, err := http.NewRequest("POST", adminURL, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("reload status %d: %s", resp.StatusCode, string(out))
	}
	return nil
}

// helpers
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
