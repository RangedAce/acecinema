package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"acecinema/pkg/auth"
	"acecinema/pkg/db"
	"acecinema/pkg/logger"
)

type config struct {
	DBURL string
	Port  string
	Token string
}

func loadConfig() (config, error) {
	cfg := config{
		DBURL: os.Getenv("DB_URL"),
		Port:  os.Getenv("PORT"),
		Token: os.Getenv("API_TOKEN"),
	}
	if cfg.DBURL == "" || cfg.Port == "" {
		return cfg, fmt.Errorf("DB_URL and PORT are required")
	}
	return cfg, nil
}

func main() {
	log := logger.New()
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("invalid config")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DBURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect db")
	}
	defer pool.Close()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(auth.TokenMiddleware(cfg.Token))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Sites
	r.Get("/sites", listSites(pool))
	r.Post("/sites", createSite(pool))
	r.Get("/sites/{id}", getSite(pool))
	r.Patch("/sites/{id}", updateSite(pool))
	r.Delete("/sites/{id}", deleteSite(pool))

	// Services
	r.Get("/services", listServices(pool))
	r.Post("/services", createService(pool))
	r.Get("/services/{id}", getService(pool))
	r.Patch("/services/{id}", updateService(pool))
	r.Delete("/services/{id}", deleteService(pool))

	// Backends
	r.Get("/backends", listBackends(pool))
	r.Post("/backends", createBackend(pool))
	r.Get("/backends/{id}", getBackend(pool))
	r.Patch("/backends/{id}", updateBackend(pool))
	r.Delete("/backends/{id}", deleteBackend(pool))

	// Health summary placeholder
	r.Get("/health/summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	addr := ":" + cfg.Port
	log.Info().Msgf("api listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal().Err(err).Msg("server stopped")
	}
}

func listSites(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), "SELECT id,name,wg_endpoint,priority,weight,created_at FROM sites ORDER BY created_at DESC")
		if err != nil {
			respondError(w, err)
			return
		}
		defer rows.Close()
		var out []Site
		for rows.Next() {
			var s Site
			if err := rows.Scan(&s.ID, &s.Name, &s.WGEndpoint, &s.Priority, &s.Weight, &s.CreatedAt); err != nil {
				respondError(w, err)
				return
			}
			out = append(out, s)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func createSite(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var s Site
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			respondError(w, err)
			return
		}
		if s.Name == "" || s.WGEndpoint == "" {
			http.Error(w, "name and wg_endpoint required", http.StatusBadRequest)
			return
		}
		err := pool.QueryRow(r.Context(), `
			INSERT INTO sites (name,wg_endpoint,priority,weight)
			VALUES ($1,$2,COALESCE($3,0),COALESCE($4,1))
			RETURNING id,created_at`,
			s.Name, s.WGEndpoint, nullInt(s.Priority), nullInt(s.Weight),
		).Scan(&s.ID, &s.CreatedAt)
		if err != nil {
			respondError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s)
	}
}

func getSite(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var s Site
		err := pool.QueryRow(r.Context(), `
			SELECT id,name,wg_endpoint,priority,weight,created_at FROM sites WHERE id=$1`, id).
			Scan(&s.ID, &s.Name, &s.WGEndpoint, &s.Priority, &s.Weight, &s.CreatedAt)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, s)
	}
}

func updateSite(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var s Site
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			respondError(w, err)
			return
		}
		_, err := pool.Exec(r.Context(), `
			UPDATE sites
			SET name=COALESCE(NULLIF($1,''),name),
			    wg_endpoint=COALESCE(NULLIF($2,''),wg_endpoint),
			    priority=COALESCE($3,priority),
			    weight=COALESCE($4,weight)
			WHERE id=$5`,
			s.Name, s.WGEndpoint, nullInt(s.Priority), nullInt(s.Weight), id)
		if err != nil {
			respondError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"updated": id})
	}
}

func deleteSite(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		_, err := pool.Exec(r.Context(), "DELETE FROM sites WHERE id=$1", id)
		if err != nil {
			respondError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func listServices(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), "SELECT id,name,domains,created_at FROM services ORDER BY created_at DESC")
		if err != nil {
			respondError(w, err)
			return
		}
		defer rows.Close()
		var out []Service
		for rows.Next() {
			var s Service
			if err := rows.Scan(&s.ID, &s.Name, &s.Domains, &s.CreatedAt); err != nil {
				respondError(w, err)
				return
			}
			out = append(out, s)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func createService(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var s Service
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			respondError(w, err)
			return
		}
		if s.Name == "" || len(s.Domains) == 0 {
			http.Error(w, "name and domains required", http.StatusBadRequest)
			return
		}
		err := pool.QueryRow(r.Context(), `
			INSERT INTO services (name,domains)
			VALUES ($1,$2)
			RETURNING id,created_at`, s.Name, s.Domains).
			Scan(&s.ID, &s.CreatedAt)
		if err != nil {
			respondError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s)
	}
}

func getService(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var s Service
		err := pool.QueryRow(r.Context(), `
			SELECT id,name,domains,created_at FROM services WHERE id=$1`, id).
			Scan(&s.ID, &s.Name, &s.Domains, &s.CreatedAt)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, s)
	}
}

func updateService(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var s Service
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			respondError(w, err)
			return
		}
		_, err := pool.Exec(r.Context(), `
			UPDATE services
			SET name=COALESCE(NULLIF($1,''),name),
			    domains=COALESCE($2,domains)
			WHERE id=$3`, s.Name, nilIfEmptySlice(s.Domains), id)
		if err != nil {
			respondError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"updated": id})
	}
}

func deleteService(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		_, err := pool.Exec(r.Context(), "DELETE FROM services WHERE id=$1", id)
		if err != nil {
			respondError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func listBackends(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id,service_id,site_id,host,port,health_path,weight,priority,is_healthy,last_checked
			FROM backends ORDER BY created_at DESC`)
		if err != nil {
			respondError(w, err)
			return
		}
		defer rows.Close()
		var out []Backend
		for rows.Next() {
			var b Backend
			if err := rows.Scan(&b.ID, &b.ServiceID, &b.SiteID, &b.Host, &b.Port, &b.HealthPath, &b.Weight, &b.Priority, &b.IsHealthy, &b.LastChecked); err != nil {
				respondError(w, err)
				return
			}
			out = append(out, b)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func createBackend(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b Backend
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			respondError(w, err)
			return
		}
		if b.ServiceID == "" || b.SiteID == "" || b.Host == "" || b.Port == 0 {
			http.Error(w, "service_id, site_id, host, port required", http.StatusBadRequest)
			return
		}
		err := pool.QueryRow(r.Context(), `
			INSERT INTO backends (service_id,site_id,host,port,health_path,weight,priority)
			VALUES ($1,$2,$3,$4,COALESCE(NULLIF($5,''),'/health'),COALESCE($6,1),COALESCE($7,0))
			RETURNING id,is_healthy,last_checked`,
			b.ServiceID, b.SiteID, b.Host, b.Port, b.HealthPath, nullInt(b.Weight), nullInt(b.Priority)).
			Scan(&b.ID, &b.IsHealthy, &b.LastChecked)
		if err != nil {
			respondError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, b)
	}
}

func getBackend(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var b Backend
		err := pool.QueryRow(r.Context(), `
			SELECT id,service_id,site_id,host,port,health_path,weight,priority,is_healthy,last_checked
			FROM backends WHERE id=$1`, id).
			Scan(&b.ID, &b.ServiceID, &b.SiteID, &b.Host, &b.Port, &b.HealthPath, &b.Weight, &b.Priority, &b.IsHealthy, &b.LastChecked)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, b)
	}
}

func updateBackend(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var b Backend
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			respondError(w, err)
			return
		}
		_, err := pool.Exec(r.Context(), `
			UPDATE backends
			SET host=COALESCE(NULLIF($1,''),host),
			    port=COALESCE(NULLIF($2,0),port),
			    health_path=COALESCE(NULLIF($3,''),health_path),
			    weight=COALESCE($4,weight),
			    priority=COALESCE($5,priority)
			WHERE id=$6`,
			b.Host, b.Port, b.HealthPath, nullInt(b.Weight), nullInt(b.Priority), id)
		if err != nil {
			respondError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"updated": id})
	}
}

func deleteBackend(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		_, err := pool.Exec(r.Context(), "DELETE FROM backends WHERE id=$1", id)
		if err != nil {
			respondError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func respondError(w http.ResponseWriter, err error) {
	http.Error(w, strings.TrimSpace(err.Error()), http.StatusInternalServerError)
}

func nullInt(v int) *int {
	return &v
}

func nilIfEmptySlice(v []string) interface{} {
	if len(v) == 0 {
		return nil
	}
	return v
}
