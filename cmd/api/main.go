package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gocql/gocql"

	"acecinema/internal/auth"
	"acecinema/internal/db"
	"acecinema/internal/media"
)

type config struct {
	Port        string
	AppSecret   string
	AdminEmail  string
	AdminPass   string
	MediaRoot   string
	TmdbKey     string
	ScyllaHosts []string
	ScyllaPort  int
	Keyspace    string
	Consistency string
	Replication int
}

type hlsSession struct {
	id         string
	userID     string
	mediaID    string
	path       string
	audioIndex int
	startSec   float64
	dir        string
	logPath    string
	cmd        *exec.Cmd
	exitErr   error
	done      chan struct{}
	lastAccess time.Time
	createdAt  time.Time
}

type hlsManager struct {
	baseDir  string
	mu       sync.Mutex
	sessions map[string]*hlsSession
	byUser   map[string]string
}

func loadConfig() (config, error) {
	hosts := strings.Split(os.Getenv("SCYLLA_HOSTS"), ",")
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}
	cfg := config{
		Port:        envDefault("API_PORT", envDefault("PORT", "8080")),
		AppSecret:   os.Getenv("APP_SECRET"),
		AdminEmail:  os.Getenv("ADMIN_EMAIL"),
		AdminPass:   os.Getenv("ADMIN_PASSWORD"),
		MediaRoot:   envDefault("MEDIA_ROOT", ""),
		TmdbKey:     os.Getenv("TMDB_API_KEY"),
		ScyllaHosts: hosts,
		ScyllaPort:  envDefaultInt("SCYLLA_PORT", 9042),
		Keyspace:    envDefault("SCYLLA_KEYSPACE", "acecinema"),
		Consistency: envDefault("SCYLLA_CONSISTENCY", "QUORUM"),
		Replication: envDefaultInt("SCYLLA_RF", 3),
	}
	if cfg.AppSecret == "" {
		return cfg, fmt.Errorf("APP_SECRET is required")
	}
	if len(cfg.ScyllaHosts) == 0 || cfg.ScyllaHosts[0] == "" {
		return cfg, fmt.Errorf("SCYLLA_HOSTS is required")
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	var session *gocql.Session
	for i := 0; i < 20; i++ {
		s, err := connectScylla(cfg)
		if err != nil {
			log.Printf("scylla connect retry %d/20: %v", i+1, err)
			time.Sleep(5 * time.Second)
			continue
		}
		if err := db.EnsureSchema(s, cfg.Keyspace); err != nil {
			s.Close()
			log.Printf("ensure schema retry %d/20: %v", i+1, err)
			time.Sleep(5 * time.Second)
			continue
		}
		if cfg.AdminEmail != "" && cfg.AdminPass != "" {
			if err := db.EnsureAdmin(context.Background(), s, cfg.Keyspace, cfg.AdminEmail, cfg.AdminPass); err != nil {
				s.Close()
				log.Printf("ensure admin retry %d/20: %v", i+1, err)
				time.Sleep(5 * time.Second)
				continue
			}
		}
		session = s
		break
	}
	if session == nil {
		log.Fatal("scylla not ready after retries")
	}
	defer session.Close()

	authSvc := auth.NewService(cfg.AppSecret)
	mediaSvc := media.NewService(session, cfg.Keyspace, cfg.MediaRoot, cfg.TmdbKey)
	hlsMgr := newHlsManager()

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Logger, middleware.Recoverer)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/", serveUI)
	r.Post("/auth/login", handleLogin(session, authSvc, cfg))
	r.Post("/auth/refresh", handleRefresh(authSvc))
	r.With(authSvc.RequireAuth).Post("/auth/change-password", handleChangePassword(session, cfg.Keyspace, authSvc))

	r.Route("/users", func(r chi.Router) {
		r.With(authSvc.RequireAuth).Get("/me", handleGetProfile(session, cfg.Keyspace))
		r.With(authSvc.RequireAuth).Put("/me", handleUpdateProfile(session, cfg.Keyspace))
		r.With(authSvc.RequireRole("admin")).Get("/", handleListUsers(session, cfg.Keyspace))
		r.With(authSvc.RequireRole("admin")).Post("/", handleCreateUser(session, cfg.Keyspace))
		r.With(authSvc.RequireRole("admin")).Put("/{id}", handleUpdateUser(session, cfg.Keyspace))
	})

	r.Route("/media", func(r chi.Router) {
		r.Use(authSvc.RequireAuth)
		r.Get("/", handleListMedia(mediaSvc))
		r.Get("/{id}", handleGetMedia(mediaSvc))
		r.Get("/{id}/assets", handleGetAssets(mediaSvc))
		r.Get("/{id}/info", handleMediaInfo(mediaSvc, session, cfg.Keyspace, cfg.MediaRoot))
		r.Get("/{id}/tracks", handleAudioTracks(mediaSvc, session, cfg.Keyspace, cfg.MediaRoot))
	})

	r.With(authSvc.RequireAuth).Put("/progress", handleUpdateProgress(mediaSvc))
	r.With(authSvc.RequireAuth).Get("/progress/continue", handleListProgress(mediaSvc, session, cfg.Keyspace, cfg.MediaRoot))

	r.Route("/admin", func(r chi.Router) {
		r.Use(authSvc.RequireRole("admin"))
		r.Get("/libraries", handleListLibraries(session, cfg.Keyspace))
		r.Post("/libraries", handleCreateLibrary(session, cfg.Keyspace))
		r.Delete("/libraries/{id}", handleDeleteLibrary(session, cfg.Keyspace))
		r.Get("/debug/tmdb", handleDebugTmdb(mediaSvc))
		r.Get("/debug/hls/{session}", handleDebugHLS(hlsMgr))
		r.Post("/scan", func(w http.ResponseWriter, r *http.Request) {
			reset := strings.TrimSpace(r.URL.Query().Get("reset"))
			if reset == "1" || strings.EqualFold(reset, "true") {
				if err := mediaSvc.Reset(r.Context()); err != nil {
					errorJSON(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			added, err := scanWithLibraries(r.Context(), mediaSvc, session, cfg.Keyspace, cfg.MediaRoot, true)
			if err != nil {
				errorJSON(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"status": "scan completed",
				"added":  added,
			})
		})
	})

	r.Get("/stream", handleStream(session, cfg.Keyspace, cfg.MediaRoot, authSvc))
	r.Get("/stream/file", handleStreamFile(session, cfg.Keyspace, cfg.MediaRoot, authSvc))
	r.With(authSvc.RequireAuth).Get("/stream/session", handleStreamSession(hlsMgr, mediaSvc, session, cfg.Keyspace, cfg.MediaRoot, authSvc))
	r.With(authSvc.RequireAuth).Get("/stream/hls", handleStreamHLS(hlsMgr, session, cfg.Keyspace, cfg.MediaRoot))
	r.Get("/stream/s/{session}/{file}", handleHLSFile(hlsMgr))
	r.Get("/hls/{session}/{file}", handleHLSFile(hlsMgr))

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			_, _ = scanWithLibraries(context.Background(), mediaSvc, session, cfg.Keyspace, cfg.MediaRoot, false)
			<-ticker.C
		}
	}()

	addr := ":" + cfg.Port
	log.Printf("api listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

func connectScylla(cfg config) (*gocql.Session, error) {
	cluster := gocql.NewCluster(cfg.ScyllaHosts...)
	cluster.Port = cfg.ScyllaPort
	cluster.Timeout = 5 * time.Second
	cluster.Consistency = parseConsistency(cfg.Consistency)

	tmpSession, err := cluster.CreateSession()
	if err != nil {
		return nil, err
	}
	defer tmpSession.Close()

	created := false
	for i := 0; i < 10; i++ {
		if err := db.EnsureKeyspace(tmpSession, cfg.Keyspace, cfg.Replication); err != nil {
			log.Printf("ensure keyspace retry %d/10: %v", i+1, err)
			time.Sleep(3 * time.Second)
			continue
		}
		created = true
		break
	}
	if !created {
		return nil, fmt.Errorf("unable to ensure keyspace %s", cfg.Keyspace)
	}

	cluster.Keyspace = cfg.Keyspace
	return cluster.CreateSession()
}

func envDefault(key, val string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return val
}

func envDefaultInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var out int
		if _, err := fmt.Sscanf(v, "%d", &out); err == nil {
			return out
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func errorJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func handleLogin(session *gocql.Session, authSvc *auth.Service, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		user, err := db.Authenticate(r.Context(), session, cfg.Keyspace, req.Email, req.Password)
		if err != nil {
			errorJSON(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		access, refresh, err := authSvc.GenerateTokens(user.ID, user.Role, user.MustChangePassword)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "token error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"access_token":  access,
			"refresh_token": refresh,
			"must_change":   user.MustChangePassword,
			"user": map[string]interface{}{
				"id":       user.ID,
				"email":    user.Email,
				"username": user.Username,
				"role":     user.Role,
			},
		})
	}
}

func handleRefresh(authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		access, refresh, err := authSvc.Refresh(req.RefreshToken)
		if err != nil {
			errorJSON(w, http.StatusUnauthorized, "invalid token")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"access_token":  access,
			"refresh_token": refresh,
		})
	}
}

func handleCreateUser(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email    string `json:"email"`
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
			Must     *bool  `json:"must_change"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		if req.Email == "" || req.Password == "" {
			errorJSON(w, http.StatusBadRequest, "email and password required")
			return
		}
		if req.Role == "" {
			req.Role = "user"
		}
		must := false
		if req.Must != nil {
			must = *req.Must
		}
		if err := db.CreateUser(r.Context(), session, keyspace, req.Email, req.Username, req.Password, req.Role, must); err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"created": req.Email})
	}
}

func handleChangePassword(session *gocql.Session, keyspace string, authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r.Context())
		var req struct {
			Old string `json:"old_password"`
			New string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		if err := db.ChangePassword(r.Context(), session, keyspace, claims.UserID, req.Old, req.New); err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "password updated"})
	}
}

func handleListMedia(svc *media.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		items, err := svc.List(r.Context(), q, 100)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func handleGetMedia(svc *media.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		item, err := svc.Get(r.Context(), id)
		if errors.Is(err, media.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

func handleGetAssets(svc *media.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		assets, err := svc.Assets(r.Context(), id)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, assets)
	}
}

func handleUpdateProgress(svc *media.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r.Context())
		var req struct {
			MediaID    string `json:"media_id"`
			PositionMs int64  `json:"position_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		if req.MediaID == "" {
			errorJSON(w, http.StatusBadRequest, "media_id required")
			return
		}
		if err := svc.UpdateProgress(r.Context(), claims.UserID, req.MediaID, req.PositionMs); err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	}
}

func handleListUsers(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := db.ListUsers(r.Context(), session, keyspace)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]map[string]interface{}, 0, len(users))
		for _, u := range users {
			out = append(out, map[string]interface{}{
				"id":          u.ID,
				"email":       u.Email,
				"username":    u.Username,
				"role":        u.Role,
				"must_change": u.MustChangePassword,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleUpdateUser(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req struct {
			Email    string `json:"email"`
			Username string `json:"username"`
			Role     string `json:"role"`
			Must     *bool  `json:"must_change"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		if strings.TrimSpace(req.Email) == "" {
			errorJSON(w, http.StatusBadRequest, "email required")
			return
		}
		role := req.Role
		if role == "" {
			role = "user"
		}
		must := false
		if req.Must != nil {
			must = *req.Must
		}
		if err := db.UpdateUser(r.Context(), session, keyspace, id, req.Email, req.Username, role, must); err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if strings.TrimSpace(req.Password) != "" {
			if err := db.UpdateUserPassword(r.Context(), session, keyspace, id, req.Password); err != nil {
				errorJSON(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	}
}

func handleGetProfile(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r.Context())
		u, err := db.GetUserByID(r.Context(), session, keyspace, claims.UserID)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":          u.ID,
			"email":       u.Email,
			"username":    u.Username,
			"role":        u.Role,
			"must_change": u.MustChangePassword,
		})
	}
}

func handleUpdateProfile(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r.Context())
		var req struct {
			Email    string `json:"email"`
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		if strings.TrimSpace(req.Email) == "" {
			errorJSON(w, http.StatusBadRequest, "email required")
			return
		}
		if err := db.UpdateProfile(r.Context(), session, keyspace, claims.UserID, req.Email, req.Username); err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	}
}

func handleListProgress(svc *media.Service, session *gocql.Session, keyspace, mediaRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil || claims.UserID == "" {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		items, err := svc.ListContinue(r.Context(), claims.UserID, 24)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		roots, err := loadLibraryRoots(r.Context(), session, keyspace, mediaRoot)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		type resp struct {
			Item        media.Item `json:"item"`
			PositionMs  int64      `json:"position_ms"`
			UpdatedAt   time.Time  `json:"updated_at"`
			DurationSec float64    `json:"duration_sec"`
		}
		out := make([]resp, 0, len(items))
		for _, entry := range items {
			duration := 0.0
			assets, err := svc.Assets(r.Context(), entry.Item.ID)
			if err == nil && len(assets) > 0 {
				if full, err := resolveStreamPath(assets[0].Path, roots); err == nil {
					if durationStr, err := probeDuration(full); err == nil {
						duration = parseDurationValue(durationStr)
					}
				}
			}
			out = append(out, resp{
				Item:        entry.Item,
				PositionMs:  entry.PositionMs,
				UpdatedAt:   entry.UpdatedAt,
				DurationSec: duration,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleListLibraries(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		libs, err := db.ListLibraries(r.Context(), session, keyspace)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, libs)
	}
}

func handleCreateLibrary(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Kind string `json:"kind"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		req.Path = strings.TrimSpace(req.Path)
		req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
		if req.Kind == "" {
			req.Kind = "movie"
		}
		if req.Kind != "movie" && req.Kind != "series" {
			errorJSON(w, http.StatusBadRequest, "kind must be movie or series")
			return
		}
		if req.Path == "" {
			errorJSON(w, http.StatusBadRequest, "path required")
			return
		}
		if req.Name == "" {
			req.Name = filepath.Base(req.Path)
		}
		lib, err := db.CreateLibrary(r.Context(), session, keyspace, req.Name, req.Path, req.Kind)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, lib)
	}
}

func handleDeleteLibrary(session *gocql.Session, keyspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			errorJSON(w, http.StatusBadRequest, "id required")
			return
		}
		if err := db.DeleteLibrary(r.Context(), session, keyspace, id); err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
	}
}

func handleDebugTmdb(svc *media.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		title := r.URL.Query().Get("title")
		yearStr := r.URL.Query().Get("year")
		imdb := r.URL.Query().Get("imdb")
		if title == "" && imdb == "" {
			errorJSON(w, http.StatusBadRequest, "title or imdb required")
			return
		}
		year := 0
		if yearStr != "" {
			if _, err := fmt.Sscanf(yearStr, "%d", &year); err != nil {
				errorJSON(w, http.StatusBadRequest, "invalid year")
				return
			}
		}
		out, err := svc.DebugTmdb(title, year, imdb)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleStream(session *gocql.Session, keyspace, mediaRoot string, authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := tokenFromRequest(r)
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if _, err := authSvc.ParseToken(token); err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			errorJSON(w, http.StatusBadRequest, "path required")
			return
		}
		roots, err := loadLibraryRoots(r.Context(), session, keyspace, mediaRoot)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		full, err := resolveStreamPath(path, roots)
		if err != nil {
			errorJSON(w, http.StatusForbidden, err.Error())
			return
		}
		http.ServeFile(w, r, full)
	}
}

func handleStreamFile(session *gocql.Session, keyspace, mediaRoot string, authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := tokenFromRequest(r)
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if _, err := authSvc.ParseToken(token); err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			errorJSON(w, http.StatusBadRequest, "path required")
			return
		}
		roots, err := loadLibraryRoots(r.Context(), session, keyspace, mediaRoot)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		full, err := resolveStreamPath(path, roots)
		if err != nil {
			errorJSON(w, http.StatusForbidden, err.Error())
			return
		}
		file, err := os.Open(full)
		if err != nil {
			errorJSON(w, http.StatusNotFound, "file not found")
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "stat failed")
			return
		}
		if ct := contentTypeForPath(full); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	}
}

func handleStreamSession(mgr *hlsManager, svc *media.Service, session *gocql.Session, keyspace, mediaRoot string, authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil || claims.UserID == "" {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		mediaID := strings.TrimSpace(r.URL.Query().Get("mediaId"))
		if mediaID == "" {
			errorJSON(w, http.StatusBadRequest, "mediaId required")
			return
		}
		startStr := strings.TrimSpace(r.URL.Query().Get("start"))
		startSec := 0.0
		if startStr != "" {
			if v, err := strconv.ParseFloat(startStr, 64); err == nil && v > 0 {
				startSec = v
			}
		}
		audioStr := strings.TrimSpace(r.URL.Query().Get("audio"))
		audioIndex := -1
		if audioStr != "" {
			if v, err := strconv.Atoi(audioStr); err == nil {
				audioIndex = v
			}
		}
		assets, err := svc.Assets(r.Context(), mediaID)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(assets) == 0 {
			errorJSON(w, http.StatusNotFound, "no assets")
			return
		}
		roots, err := loadLibraryRoots(r.Context(), session, keyspace, mediaRoot)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		full, err := resolveStreamPath(assets[0].Path, roots)
		if err != nil {
			errorJSON(w, http.StatusForbidden, err.Error())
			return
		}
		durationStr, _ := probeDuration(full)
		duration := parseDurationValue(durationStr)
		if ok, _ := isDirectPlayable(full); ok {
			token := tokenFromRequest(r)
			if token == "" {
				http.Error(w, "missing token", http.StatusUnauthorized)
				return
			}
			streamURL := "/stream/file?path=" + url.QueryEscape(assets[0].Path) + "&token=" + url.QueryEscape(token)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"mode":     "direct",
				"url":      streamURL,
				"duration": duration,
			})
			return
		}
		sess, err := mgr.CreateForUser(claims.UserID, mediaID, full, audioIndex, startSec)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		indexPath := filepath.Join(sess.dir, "index.m3u8")
		status := "ready"
		if !waitForFile(indexPath, 20*time.Second) {
			if sess.isDone() {
				logMsg := readLogSnippet(sess.logPath, 4000)
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"error":   "ffmpeg exited",
					"session": sess.id,
					"log":     logMsg,
				})
				return
			}
			status = "starting"
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"mode":     "hls",
			"url":      "/stream/s/" + sess.id + "/index.m3u8",
			"session":  sess.id,
			"duration": duration,
			"status":   status,
			"start":    startSec,
		})
	}
}

func handleStreamHLS(mgr *hlsManager, session *gocql.Session, keyspace, mediaRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			errorJSON(w, http.StatusBadRequest, "path required")
			return
		}
		audioStr := r.URL.Query().Get("audio")
		audioIndex := -1
		if audioStr != "" {
			if v, err := strconv.Atoi(audioStr); err == nil {
				audioIndex = v
			}
		}
		startStr := r.URL.Query().Get("start")
		startSec := 0.0
		if startStr != "" {
			if v, err := strconv.ParseFloat(startStr, 64); err == nil && v > 0 {
				startSec = v
			}
		}
		roots, err := loadLibraryRoots(r.Context(), session, keyspace, mediaRoot)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		full, err := resolveStreamPath(path, roots)
		if err != nil {
			errorJSON(w, http.StatusForbidden, err.Error())
			return
		}
		duration, _ := probeDuration(full)
		sess, err := mgr.Create(full, audioIndex, startSec)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		indexPath := filepath.Join(sess.dir, "index.m3u8")
		ready := waitForFile(indexPath, 20*time.Second)
		if !ready {
			logMsg := readLogSnippet(sess.logPath, 4000)
			if sess.isDone() {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"error":   "ffmpeg exited",
					"session": sess.id,
					"log":     logMsg,
				})
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]interface{}{
				"status":   "starting",
				"url":      "/hls/" + sess.id + "/index.m3u8",
				"session":  sess.id,
				"duration": duration,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":   "ready",
			"url":      "/hls/" + sess.id + "/index.m3u8",
			"session":  sess.id,
			"duration": duration,
		})
	}
}

func handleHLSFile(mgr *hlsManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "session")
		file := chi.URLParam(r, "file")
		if sessionID == "" || file == "" {
			http.NotFound(w, r)
			return
		}
		sess := mgr.Get(sessionID)
		if sess == nil {
			http.NotFound(w, r)
			return
		}
		mgr.Touch(sessionID)
		clean := filepath.Clean(file)
		if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(sess.dir, clean)
		if !waitForFile(full, 40*time.Second) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		if strings.HasSuffix(clean, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		} else if strings.HasSuffix(clean, ".ts") {
			w.Header().Set("Content-Type", "video/mp2t")
		}
		http.ServeFile(w, r, full)
	}
}

func handleAudioTracks(svc *media.Service, session *gocql.Session, keyspace, mediaRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			errorJSON(w, http.StatusBadRequest, "id required")
			return
		}
		assets, err := svc.Assets(r.Context(), id)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(assets) == 0 {
			errorJSON(w, http.StatusNotFound, "no assets")
			return
		}
		roots, err := loadLibraryRoots(r.Context(), session, keyspace, mediaRoot)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		full, err := resolveStreamPath(assets[0].Path, roots)
		if err != nil {
			errorJSON(w, http.StatusForbidden, err.Error())
			return
		}
		tracks, err := probeAudioTracks(full)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, tracks)
	}
}

type videoInfo struct {
	Codec     string `json:"codec"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	FrameRate string `json:"frame_rate"`
}

type mediaInfo struct {
	Duration float64     `json:"duration"`
	Video    videoInfo   `json:"video"`
	Audio    []audioTrack `json:"audio_tracks"`
}

func handleMediaInfo(svc *media.Service, session *gocql.Session, keyspace, mediaRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			errorJSON(w, http.StatusBadRequest, "id required")
			return
		}
		assets, err := svc.Assets(r.Context(), id)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(assets) == 0 {
			errorJSON(w, http.StatusNotFound, "no assets")
			return
		}
		roots, err := loadLibraryRoots(r.Context(), session, keyspace, mediaRoot)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		full, err := resolveStreamPath(assets[0].Path, roots)
		if err != nil {
			errorJSON(w, http.StatusForbidden, err.Error())
			return
		}
		durationStr, _ := probeDuration(full)
		duration, _ := strconv.ParseFloat(durationStr, 64)
		video, _ := probeVideoInfo(full)
		audio, _ := probeAudioTracks(full)
		writeJSON(w, http.StatusOK, mediaInfo{
			Duration: duration,
			Video:    video,
			Audio:    audio,
		})
	}
}

func handleDebugHLS(mgr *hlsManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "session")
		if sessionID == "" {
			errorJSON(w, http.StatusBadRequest, "session required")
			return
		}
		sess := mgr.Get(sessionID)
		if sess == nil {
			errorJSON(w, http.StatusNotFound, "session not found")
			return
		}
		data, err := os.ReadFile(sess.logPath)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"log": string(data),
		})
	}
}

func tokenFromRequest(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if authz != "" {
		parts := strings.SplitN(authz, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return parts[1]
		}
	}
	return r.URL.Query().Get("token")
}

func scanWithLibraries(ctx context.Context, svc *media.Service, session *gocql.Session, keyspace, fallback string, refreshExisting bool) (int, error) {
	libs, err := loadLibraries(ctx, session, keyspace, fallback)
	if err != nil {
		return 0, err
	}
	if len(libs) == 0 {
		return 0, nil
	}
	roots := make([]media.LibraryRoot, 0, len(libs))
	for _, lib := range libs {
		roots = append(roots, media.LibraryRoot{Path: lib.Path, Kind: lib.Kind})
	}
	return svc.ScanLibraries(ctx, roots, refreshExisting)
}

func loadLibraries(ctx context.Context, session *gocql.Session, keyspace, fallback string) ([]db.Library, error) {
	libs, err := db.ListLibraries(ctx, session, keyspace)
	if err != nil {
		return nil, err
	}
	filtered := make([]db.Library, 0, len(libs))
	for _, lib := range libs {
		if strings.TrimSpace(lib.Path) != "" {
			if lib.Kind == "" {
				lib.Kind = "movie"
			}
			filtered = append(filtered, lib)
		}
	}
	if len(filtered) == 0 && fallback != "" {
		filtered = append(filtered, db.Library{Path: fallback, Kind: "movie"})
	}
	return filtered, nil
}

func loadLibraryRoots(ctx context.Context, session *gocql.Session, keyspace, fallback string) ([]string, error) {
	libs, err := loadLibraries(ctx, session, keyspace, fallback)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(libs))
	for _, lib := range libs {
		if strings.TrimSpace(lib.Path) != "" {
			roots = append(roots, lib.Path)
		}
	}
	return roots, nil
}

func resolveStreamPath(path string, roots []string) (string, error) {
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) {
		for _, root := range roots {
			root = filepath.Clean(root)
			if root == "" {
				continue
			}
			if isWithinRoot(clean, root) {
				return clean, nil
			}
		}
		return "", fmt.Errorf("path not allowed")
	}
	for _, root := range roots {
		root = filepath.Clean(root)
		if root == "" {
			continue
		}
		full := filepath.Join(root, clean)
		if _, err := os.Stat(full); err == nil {
			return full, nil
		}
	}
	return "", fmt.Errorf("file not found")
}

func isWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

type audioTrack struct {
	Index      int    `json:"index"`
	AudioIndex int    `json:"audio_index"`
	Codec      string `json:"codec"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
	Language   string `json:"language"`
	Title      string `json:"title"`
}

func probeDuration(path string) (string, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration:stream=duration",
		"-of", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var parsed struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Duration string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", err
	}
	best := parseDurationValue(parsed.Format.Duration)
	for _, s := range parsed.Streams {
		if d := parseDurationValue(s.Duration); d > best {
			best = d
		}
	}
	if best <= 0 {
		return "", fmt.Errorf("duration not found")
	}
	return fmt.Sprintf("%.3f", best), nil
}

func parseDurationValue(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "N/A" {
		return 0
	}
	val, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return val
}

func probeAudioTracks(path string) ([]audioTrack, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "a",
		"-show_entries", "stream=index,codec_name,channels,sample_rate:stream_tags=language,title",
		"-of", "json", path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Streams []struct {
			Index      int               `json:"index"`
			CodecName  string            `json:"codec_name"`
			Channels   int               `json:"channels"`
			SampleRate string            `json:"sample_rate"`
			Tags       map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	tracks := make([]audioTrack, 0, len(parsed.Streams))
	for i, s := range parsed.Streams {
		track := audioTrack{
			Index:      s.Index,
			AudioIndex: i,
			Codec:      s.CodecName,
			Channels:   s.Channels,
			SampleRate: s.SampleRate,
			Language:   s.Tags["language"],
			Title:      s.Tags["title"],
		}
		tracks = append(tracks, track)
	}
	return tracks, nil
}

func probeVideoInfo(path string) (videoInfo, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,avg_frame_rate",
		"-of", "json", path,
	)
	out, err := cmd.Output()
	if err != nil {
		return videoInfo{}, err
	}
	var parsed struct {
		Streams []struct {
			CodecName   string `json:"codec_name"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			AvgFrameRate string `json:"avg_frame_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return videoInfo{}, err
	}
	if len(parsed.Streams) == 0 {
		return videoInfo{}, fmt.Errorf("no video stream")
	}
	s := parsed.Streams[0]
	return videoInfo{
		Codec:     s.CodecName,
		Width:     s.Width,
		Height:    s.Height,
		FrameRate: s.AvgFrameRate,
	}, nil
}

func probeContainer(path string) (string, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=format_name",
		"-of", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var parsed struct {
		Format struct {
			FormatName string `json:"format_name"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", err
	}
	return parsed.Format.FormatName, nil
}

func isDirectPlayable(path string) (bool, string) {
	format, err := probeContainer(path)
	if err != nil {
		return false, "format"
	}
	video, err := probeVideoInfo(path)
	if err != nil {
		return false, "video"
	}
	audio, _ := probeAudioTracks(path)
	containerOK := false
	allowed := map[string]bool{"mp4": true, "mov": true, "m4v": true}
	for _, part := range strings.Split(format, ",") {
		if allowed[strings.TrimSpace(part)] {
			containerOK = true
			break
		}
	}
	videoOK := strings.EqualFold(video.Codec, "h264")
	audioOK := true
	if len(audio) > 0 {
		codec := strings.ToLower(audio[0].Codec)
		audioOK = codec == "aac" || codec == "mp3"
	}
	if !containerOK {
		return false, "container"
	}
	if !videoOK {
		return false, "video"
	}
	if !audioOK {
		return false, "audio"
	}
	return true, ""
}

func contentTypeForPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".m4v", ".mov":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	}
	return mime.TypeByExtension(ext)
}

func newHlsManager() *hlsManager {
	base := "/tmp/acecinema-hls"
	_ = os.MkdirAll(base, 0o755)
	m := &hlsManager{
		baseDir:  base,
		sessions: make(map[string]*hlsSession),
		byUser:   make(map[string]string),
	}
	go m.cleanupLoop()
	return m
}

func (m *hlsManager) Create(path string, audioIndex int, startSec float64) (*hlsSession, error) {
	return m.createSession("", "", path, audioIndex, startSec)
}

func (m *hlsManager) CreateForUser(userID, mediaID, path string, audioIndex int, startSec float64) (*hlsSession, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("user required")
	}
	return m.createSession(userID, mediaID, path, audioIndex, startSec)
}

func (m *hlsManager) createSession(userID, mediaID, path string, audioIndex int, startSec float64) (*hlsSession, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	id := randomID()
	dir := filepath.Join(m.baseDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sess := &hlsSession{
		id:         id,
		userID:     userID,
		mediaID:    mediaID,
		path:       path,
		audioIndex: audioIndex,
		startSec:   startSec,
		dir:        dir,
		logPath:    filepath.Join(dir, "ffmpeg.log"),
		done:       make(chan struct{}),
		lastAccess: time.Now(),
		createdAt:  time.Now(),
	}
	m.mu.Lock()
	if userID != "" {
		if existingID, ok := m.byUser[userID]; ok {
			m.removeLocked(existingID)
		}
		m.byUser[userID] = id
	}
	m.sessions[id] = sess
	m.mu.Unlock()
	go m.startSession(sess)
	return sess, nil
}

func (m *hlsManager) Get(id string) *hlsSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *hlsManager) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.cleanupOld(3 * time.Minute)
	}
}

func (m *hlsManager) cleanupOld(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, sess := range m.sessions {
		if now.Sub(sess.lastAccess) > maxAge {
			m.removeLocked(id)
		}
	}
}

func (m *hlsManager) removeLocked(id string) {
	sess, ok := m.sessions[id]
	if !ok {
		return
	}
	if sess.cmd != nil && sess.cmd.Process != nil {
		_ = sess.cmd.Process.Kill()
	}
	_ = os.RemoveAll(sess.dir)
	delete(m.sessions, id)
	if sess.userID != "" {
		if current, ok := m.byUser[sess.userID]; ok && current == id {
			delete(m.byUser, sess.userID)
		}
	}
}

func (m *hlsManager) Touch(id string) {
	m.mu.Lock()
	if sess, ok := m.sessions[id]; ok {
		sess.lastAccess = time.Now()
	}
	m.mu.Unlock()
}

func (m *hlsManager) startSession(sess *hlsSession) {
	log.Printf("hls start: user=%s media=%s path=%s audio=%d start=%.3f dir=%s", sess.userID, sess.mediaID, sess.path, sess.audioIndex, sess.startSec, sess.dir)
	mapAudio := "0:a:0?"
	if sess.audioIndex >= 0 {
		mapAudio = fmt.Sprintf("0:a:%d?", sess.audioIndex)
	}
	audioCopy := false
	if tracks, err := probeAudioTracks(sess.path); err == nil && len(tracks) > 0 {
		idx := 0
		if sess.audioIndex >= 0 && sess.audioIndex < len(tracks) {
			idx = sess.audioIndex
		}
		codec := strings.ToLower(tracks[idx].Codec)
		if codec == "aac" || codec == "mp3" {
			audioCopy = true
		}
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-analyzeduration", "20M",
		"-probesize", "20M",
	}
	if sess.startSec > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", sess.startSec))
	}
	args = append(args,
		"-i", sess.path,
		"-map", "0:v:0",
		"-map", mapAudio,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-crf", "26",
		"-tune", "zerolatency",
		"-g", "48",
		"-keyint_min", "48",
		"-sc_threshold", "0",
		"-max_muxing_queue_size", "1024",
	)
	if audioCopy {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", "160k")
	}
	args = append(args,
		"-sn",
		"-f", "hls",
		"-hls_time", "4",
		"-hls_list_size", "10",
		"-hls_flags", "independent_segments+delete_segments",
		"-hls_segment_filename", filepath.Join(sess.dir, "seg%03d.ts"),
		filepath.Join(sess.dir, "index.m3u8"),
	)
	cmd := exec.Command("ffmpeg", args...)
	// prevent ffmpeg from hanging on stdin
	stdin, err := os.Open(os.DevNull)
	if err == nil {
		cmd.Stdin = stdin
		defer stdin.Close()
	}

	logFile, err := os.OpenFile(sess.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	sess.cmd = cmd
	if err := cmd.Run(); err != nil {
		log.Printf("hls ffmpeg exit: %v", err)
		sess.exitErr = err
	}
	close(sess.done)
}

func (s *hlsSession) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func waitForFile(path string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func readLogSnippet(path string, max int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "no log"
	}
	if len(data) <= max {
		return string(data)
	}
	return string(data[len(data)-max:])
}

func parseConsistency(c string) gocql.Consistency {
	switch strings.ToUpper(c) {
	case "ONE":
		return gocql.One
	case "LOCAL_ONE":
		return gocql.LocalOne
	case "LOCAL_QUORUM":
		return gocql.LocalQuorum
	case "ALL":
		return gocql.All
	default:
		return gocql.Quorum
	}
}

func serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	debugSeek := envDefault("DEBUG_SEEK", "") == "1"
	fmt.Fprint(w, `<!doctype html>
<html>
<head>
  <meta charset="utf-8"/>
  <title>AceCinema</title>
  <script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.12"></script>
  <style>
    @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&family=Poppins:wght@600;700&display=swap');
    :root {
      --bg-primary: #0A0E27;
      --bg-secondary: #161B33;
      --accent: #6366F1;
      --accent-2: #EC4899;
      --text: #F3F4F6;
      --text-muted: #9CA3AF;
      --success: #10B981;
      --error: #EF4444;
      --radius-card: 12px;
      --radius-btn: 6px;
      --glass: rgba(22, 27, 51, 0.8);
      --shadow-accent: 0 10px 40px rgba(99, 102, 241, 0.3);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      color: var(--text);
      background: linear-gradient(135deg, #0A0E27 0%, #161B33 100%);
      font-family: "Inter", "Noto Sans", system-ui, sans-serif;
      font-size: 15px;
      overflow-x: hidden;
    }
    h1, h2, .logo, .brand {
      font-family: "Poppins", "Inter", sans-serif;
    }
    code, .mono {
      font-family: "JetBrains Mono", ui-monospace, monospace;
    }
    input, button {
      font-family: inherit;
    }
    .hidden { display: none !important; }
    .app {
      display: flex;
      min-height: 100vh;
      width: 100%;
    }
    .sidebar {
      width: 240px;
      background: rgba(22, 27, 51, 0.95);
      border-right: 1px solid rgba(255, 255, 255, 0.08);
      padding: 24px 18px;
      position: sticky;
      top: 0;
      height: 100vh;
      display: flex;
      flex-direction: column;
      gap: 20px;
      transition: transform 0.2s ease;
    }
    .brand {
      font-size: 20px;
      font-weight: 700;
      letter-spacing: 0.3px;
    }
    .nav {
      display: flex;
      flex-direction: column;
      gap: 10px;
    }
    .nav button {
      background: transparent;
      border: 1px solid transparent;
      color: var(--text-muted);
      padding: 10px 12px;
      border-radius: 10px;
      text-align: left;
      display: flex;
      align-items: center;
      gap: 10px;
      cursor: pointer;
      transition: all 0.2s ease;
    }
    .nav button svg {
      width: 18px;
      height: 18px;
      stroke: currentColor;
    }
    .nav button:hover,
    .nav button.active {
      color: var(--text);
      background: rgba(99, 102, 241, 0.16);
      border-color: rgba(99, 102, 241, 0.35);
      box-shadow: 0 10px 30px rgba(99, 102, 241, 0.15);
    }
    .main {
      flex: 1;
      padding: 24px 28px 32px;
      min-width: 0;
    }
    .topbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 24px;
    }
    .topbar-left {
      display: flex;
      align-items: center;
      gap: 12px;
      flex: 1;
    }
    .sidebar-toggle {
      display: none;
      width: 40px;
      height: 40px;
      border-radius: 8px;
      border: 1px solid rgba(255, 255, 255, 0.1);
      background: rgba(22, 27, 51, 0.7);
      color: var(--text);
      cursor: pointer;
    }
    .search {
      flex: 1;
      max-width: 520px;
    }
    .search input {
      width: 100%;
      padding: 12px 16px;
      border-radius: 10px;
      border: 1px solid rgba(255, 255, 255, 0.1);
      background: var(--bg-secondary);
      color: var(--text);
      transition: border 0.2s ease, box-shadow 0.2s ease;
    }
    .search input:focus {
      border-color: rgba(99, 102, 241, 0.6);
      box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.2);
      outline: none;
    }
    .avatar-wrap {
      position: relative;
    }
    .avatar {
      width: 40px;
      height: 40px;
      border-radius: 50%;
      border: 1px solid rgba(255, 255, 255, 0.1);
      background: var(--accent);
      color: var(--text);
      cursor: pointer;
      transition: transform 0.2s ease;
    }
    .avatar:hover { transform: scale(1.03); }
    .menu {
      position: absolute;
      right: 0;
      top: 48px;
      background: var(--bg-secondary);
      border: 1px solid rgba(255, 255, 255, 0.08);
      border-radius: 12px;
      padding: 8px;
      min-width: 180px;
      display: flex;
      flex-direction: column;
      gap: 6px;
      z-index: 10;
      box-shadow: 0 20px 50px rgba(3, 6, 20, 0.6);
    }
    .menu button {
      background: transparent;
      border: none;
      text-align: left;
      padding: 8px 10px;
      border-radius: 8px;
      cursor: pointer;
      color: var(--text);
      transition: all 0.2s ease;
    }
    .menu button:hover {
      background: rgba(99, 102, 241, 0.2);
    }
    .avatar-grid {
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
      margin-top: 10px;
    }
    .avatar-option {
      width: 34px;
      height: 34px;
      border-radius: 50%;
      border: 1px solid rgba(255, 255, 255, 0.2);
      cursor: pointer;
    }
    .panel {
      border: 1px solid rgba(255, 255, 255, 0.08);
      border-radius: 12px;
      padding: 16px;
      background: rgba(22, 27, 51, 0.65);
      backdrop-filter: blur(10px);
      box-shadow: 0 20px 50px rgba(10, 14, 39, 0.4);
    }
    .auth-panel {
      max-width: 520px;
      margin: 80px auto 0;
    }
    .row { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
    .checkbox {
      display: flex;
      align-items: center;
      gap: 6px;
      color: var(--text-muted);
      font-size: 12px;
    }
    .checkbox input {
      min-width: auto;
    }
    input {
      padding: 10px 12px;
      border-radius: 10px;
      border: 1px solid rgba(255, 255, 255, 0.08);
      background: var(--bg-secondary);
      color: var(--text);
      min-width: 220px;
    }
    select {
      padding: 10px 12px;
      border-radius: 10px;
      border: 1px solid rgba(255, 255, 255, 0.08);
      background: var(--bg-secondary);
      color: var(--text);
      min-width: 160px;
    }
    button {
      background: var(--accent);
      color: var(--text);
      border: 1px solid transparent;
      padding: 10px 18px;
      border-radius: var(--radius-btn);
      cursor: pointer;
      transition: all 0.2s ease;
      font-weight: 600;
    }
    button.secondary {
      background: transparent;
      border: 1px solid rgba(255, 255, 255, 0.15);
      color: var(--text);
    }
    button:hover {
      transform: translateY(-1px);
      box-shadow: 0 10px 30px rgba(99, 102, 241, 0.3);
    }
    button:disabled {
      opacity: 0.5;
      cursor: not-allowed;
      box-shadow: none;
      transform: none;
    }
    .actions { margin-top: 12px; }
    .view { display: flex; flex-direction: column; gap: 24px; }
    .section { display: flex; flex-direction: column; gap: 14px; }
    .hero {
      position: relative;
      min-height: 280px;
      border-radius: 16px;
      overflow: hidden;
      background: radial-gradient(circle at top, rgba(99, 102, 241, 0.35), transparent 60%), var(--bg-secondary);
      display: flex;
      align-items: flex-end;
      transition: background 0.3s ease;
    }
    .hero::before {
      content: "";
      position: absolute;
      inset: 0;
      background-image: var(--hero-image, none);
      background-size: 100% auto;
      background-position: center top;
      background-repeat: no-repeat;
      filter: saturate(1.1);
      opacity: 0.65;
    }
    .hero::after {
      content: "";
      position: absolute;
      inset: 0;
      background: linear-gradient(180deg, rgba(10, 14, 39, 0.2) 0%, rgba(10, 14, 39, 0.9) 85%);
    }
    .hero-content {
      position: relative;
      z-index: 1;
      padding: 28px;
      max-width: 520px;
      display: flex;
      flex-direction: column;
      gap: 12px;
      transition: opacity 0.35s ease, transform 0.35s ease;
    }
    .hero.fade .hero-content {
      opacity: 0;
      transform: translateY(6px);
    }
    .hero.slide::before {
      animation: hero-pan 7s ease both;
    }
    @keyframes hero-pan {
      0% { transform: scale(1.02) translateX(0); }
      100% { transform: scale(1.04) translateX(-2%); }
    }
    .hero-title {
      font-size: 30px;
      font-weight: 700;
    }
    .hero-meta {
      color: var(--text-muted);
      font-size: 13px;
      letter-spacing: 0.4px;
      text-transform: uppercase;
    }
    .hero-desc {
      color: var(--text-muted);
      line-height: 1.5;
    }
    .hero-actions {
      display: flex;
      gap: 12px;
      margin-top: 8px;
    }
    .section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
    }
    .section-title {
      font-size: 18px;
      font-weight: 600;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(210px, 1fr));
      gap: 16px;
    }
    .user-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
      gap: 16px;
      margin-top: 12px;
    }
    .media-row {
      display: grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(210px, 250px);
      gap: 16px;
      overflow-x: auto;
      padding-bottom: 8px;
      scroll-snap-type: x mandatory;
    }
    .media-row::-webkit-scrollbar {
      height: 8px;
    }
    .media-row::-webkit-scrollbar-thumb {
      background: rgba(99, 102, 241, 0.4);
      border-radius: 999px;
    }
    .media-row .card {
      scroll-snap-align: start;
    }
    .card {
      border-radius: var(--radius-card);
      overflow: hidden;
      background: rgba(22, 27, 51, 0.8);
      border: 1px solid rgba(255, 255, 255, 0.08);
      position: relative;
      display: flex;
      flex-direction: column;
      gap: 10px;
      padding: 12px;
      transition: all 0.2s ease;
      cursor: pointer;
    }
    .user-card {
      cursor: default;
    }
    .user-card:hover {
      transform: none;
      box-shadow: none;
      border-color: rgba(255, 255, 255, 0.08);
    }
    .user-card .row {
      gap: 8px;
    }
    .user-label {
      font-size: 12px;
      color: var(--text-muted);
      text-transform: uppercase;
      letter-spacing: 0.4px;
    }
    .card:hover {
      transform: scale(1.03);
      box-shadow: var(--shadow-accent);
      border-color: rgba(99, 102, 241, 0.4);
    }
    .poster {
      width: 100%;
      aspect-ratio: 2 / 3;
      background: rgba(22, 27, 51, 0.9);
      border-radius: 8px;
      object-fit: cover;
    }
    .card-title {
      font-weight: 600;
      font-size: 14px;
    }
    .card-year {
      color: var(--text-muted);
      font-size: 12px;
    }
    .play-btn {
      width: 36px;
      height: 36px;
      border-radius: 50%;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0;
      background: rgba(99, 102, 241, 0.2);
      border: 1px solid rgba(99, 102, 241, 0.5);
    }
    .badge {
      position: absolute;
      top: 10px;
      right: 10px;
      background: rgba(22, 27, 51, 0.8);
      color: var(--text);
      border: 1px solid rgba(255, 255, 255, 0.15);
      padding: 4px 6px;
      border-radius: 6px;
      font-size: 10px;
      text-transform: uppercase;
      letter-spacing: 0.5px;
    }
    .rating-badge {
      position: absolute;
      top: 10px;
      left: 10px;
      background: rgba(10, 14, 39, 0.85);
      color: var(--text-main);
      border: 1px solid rgba(99, 102, 241, 0.4);
      padding: 4px 6px;
      border-radius: 6px;
      font-size: 10px;
      letter-spacing: 0.4px;
    }
    .card-remove {
      position: absolute;
      top: 10px;
      right: 10px;
      width: 26px;
      height: 26px;
      border-radius: 999px;
      border: 1px solid rgba(255, 255, 255, 0.2);
      background: rgba(10, 14, 39, 0.7);
      color: var(--text-main);
      font-size: 14px;
      line-height: 1;
      padding: 0;
      display: flex;
      align-items: center;
      justify-content: center;
      cursor: pointer;
      z-index: 2;
      transition: all 0.2s ease;
    }
    .card-remove:hover {
      border-color: rgba(236, 72, 153, 0.6);
      color: #EC4899;
    }
    .progress {
      height: 4px;
      width: 100%;
      background: rgba(255, 255, 255, 0.08);
      border-radius: 999px;
      overflow: hidden;
      margin-top: 6px;
    }
    .progress span {
      display: block;
      height: 100%;
      background: var(--accent);
      width: 0%;
    }
    .details-overlay {
      position: fixed;
      inset: 0;
      background: rgba(10, 14, 39, 0.7);
      display: flex;
      align-items: center;
      justify-content: center;
      z-index: 998;
      padding: 24px;
      backdrop-filter: blur(8px);
    }
    .details-modal {
      width: min(1100px, 95%);
      max-height: 90vh;
      overflow-y: auto;
      background: var(--glass);
      border: 1px solid rgba(255, 255, 255, 0.1);
      border-radius: 16px;
      padding: 24px;
      box-shadow: 0 30px 80px rgba(3, 6, 20, 0.6);
      display: flex;
      flex-direction: column;
      gap: 20px;
    }
    .details-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 16px;
    }
    .details-title {
      font-size: 28px;
      font-weight: 700;
    }
    .details-original {
      color: var(--text-muted);
      font-size: 13px;
      margin-top: 4px;
    }
    .details-close {
      background: rgba(236, 72, 153, 0.2);
      border: 1px solid rgba(236, 72, 153, 0.5);
      color: var(--text);
      padding: 8px 12px;
      border-radius: 8px;
      cursor: pointer;
    }
    .details-body {
      display: grid;
      grid-template-columns: minmax(200px, 260px) 1fr;
      gap: 24px;
    }
    .details-poster {
      width: 100%;
      border-radius: 12px;
      object-fit: cover;
      aspect-ratio: 2 / 3;
      background: rgba(22, 27, 51, 0.8);
    }
    .details-meta {
      display: flex;
      flex-wrap: wrap;
      gap: 12px;
      color: var(--text-muted);
      font-size: 13px;
      margin-top: 6px;
    }
    .details-tagline {
      color: var(--accent-2);
      font-style: italic;
      margin-top: 8px;
    }
    .details-synopsis {
      margin-top: 10px;
      line-height: 1.6;
      color: var(--text);
    }
    .details-actions {
      display: flex;
      gap: 12px;
      margin-top: 16px;
    }
    .details-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 12px;
      margin-top: 16px;
    }
    .details-grid .label {
      font-size: 11px;
      text-transform: uppercase;
      letter-spacing: 0.6px;
      color: var(--text-muted);
      margin-bottom: 4px;
    }
    .details-chips {
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      margin-top: 10px;
    }
    .chip {
      background: rgba(99, 102, 241, 0.2);
      border: 1px solid rgba(99, 102, 241, 0.4);
      border-radius: 999px;
      padding: 4px 10px;
      font-size: 11px;
    }
    .cast-row {
      display: grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(140px, 170px);
      gap: 12px;
      overflow-x: auto;
      padding-bottom: 8px;
    }
    .cast-card {
      background: rgba(22, 27, 51, 0.8);
      border: 1px solid rgba(255, 255, 255, 0.08);
      border-radius: 12px;
      padding: 10px;
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    .cast-name {
      font-weight: 600;
      font-size: 13px;
    }
    .cast-role {
      color: var(--text-muted);
      font-size: 12px;
    }
    .meta { color: var(--text-muted); font-size: 12px; }
    .player-overlay {
      position: fixed;
      inset: 0;
      background: rgba(0, 0, 0, 0.75);
      display: none;
      align-items: center;
      justify-content: center;
      z-index: 999;
      padding: 0;
      backdrop-filter: blur(6px);
    }
    .player-shell {
      width: 100vw;
      height: 100vh;
      background: #000;
      border-radius: 0;
      overflow: hidden;
      border: 1px solid rgba(255, 255, 255, 0.1);
      position: relative;
      display: flex;
      flex-direction: column;
      max-height: 100vh;
      backdrop-filter: blur(20px);
    }
    .no-scroll {
      overflow: hidden;
    }
    .player-shell:not(.controls-visible) {
      cursor: none;
    }
    .player-shell:not(.controls-visible) * {
      cursor: none;
    }
    .player-shell.controls-visible .player-bar,
    .player-shell.controls-visible .player-controls {
      opacity: 1;
      pointer-events: auto;
    }
    .player-shell.is-fullscreen {
      width: 100%;
      height: 100%;
      border-radius: 0;
    }
    .player-shell.is-fullscreen video {
      width: 100%;
      height: 100%;
      object-fit: contain;
    }
    .player-bar {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding: 10px 14px;
      color: #fff;
      font-size: 13px;
      position: absolute;
      left: 0;
      right: 0;
      top: 0;
      opacity: 0;
      pointer-events: none;
      transition: opacity 0.2s ease;
      z-index: 2;
      background: rgba(0, 0, 0, 0.6);
    }
    .player-close {
      background: rgba(236, 72, 153, 0.2);
      color: #fff;
      border: 1px solid rgba(236, 72, 153, 0.5);
      cursor: pointer;
      flex-shrink: 0;
      border-radius: 8px;
      padding: 6px 10px;
    }
    .player-controls {
      display: flex;
      gap: 10px;
      align-items: center;
      padding: 10px 14px;
      background: rgba(0, 0, 0, 0.55);
      color: #fff;
      font-size: 12px;
      position: absolute;
      left: 0;
      right: 0;
      bottom: 0;
      opacity: 0;
      pointer-events: none;
      transition: opacity 0.2s ease;
      z-index: 2;
    }
    .player-loading {
      position: absolute;
      inset: 0;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 12px;
      background: rgba(10, 14, 39, 0.4);
      opacity: 0;
      pointer-events: none;
      transition: opacity 0.2s ease;
      z-index: 3;
    }
    .player-loading.active {
      opacity: 1;
    }
    .player-loading-text {
      color: var(--text-main);
      font-size: 12px;
      letter-spacing: 0.4px;
      text-transform: uppercase;
    }
    .spinner {
      width: 52px;
      height: 52px;
      position: relative;
      transform-origin: 50% 50%;
      animation: spinner-rotate 1.1s linear infinite;
    }
    .spinner span {
      position: absolute;
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--accent);
      top: 50%;
      left: 50%;
      transform: translate(-50%, -50%) rotate(calc(var(--i) * 45deg)) translate(18px);
      opacity: calc(0.2 + (var(--i) / 12));
    }
    @keyframes spinner-rotate {
      from { transform: rotate(0deg); }
      to { transform: rotate(360deg); }
    }
    .seek-wrap {
      position: relative;
      flex: 1;
      display: flex;
      align-items: center;
    }
    .player-ctrl {
      background: rgba(99, 102, 241, 0.2);
      color: #fff;
      border: 1px solid rgba(99, 102, 241, 0.6);
      border-radius: 8px;
      padding: 6px 10px;
      cursor: pointer;
      transition: all 0.2s ease;
    }
    .player-ctrl:active,
    .player-close:active {
      transform: translateY(1px);
    }
    .seek-bar { width: 100%; }
    .volume-bar { width: 110px; }
    .time-label { min-width: 120px; color: #cfcfcf; }
    .seek-preview {
      position: absolute;
      bottom: 18px;
      transform: translateX(-50%);
      background: rgba(10, 14, 39, 0.9);
      border: 1px solid rgba(255, 255, 255, 0.12);
      color: var(--text-main);
      font-size: 12px;
      padding: 4px 8px;
      border-radius: 6px;
      opacity: 0;
      pointer-events: none;
      transition: opacity 0.12s ease;
      white-space: nowrap;
    }
    .seek-preview.visible {
      opacity: 1;
    }
    input[type=range] {
      -webkit-appearance: none;
      appearance: none;
      height: 6px;
      background: transparent;
      outline: none;
      padding: 0;
      margin: 0;
      border: none;
      --thumb-size: 14px;
      --track-color: rgba(255, 255, 255, 0.2);
      --fill-color: #6366F1;
      --fill-percent: 0%;
    }
    input[type=range]::-webkit-slider-runnable-track {
      height: 6px;
      border-radius: 999px;
      background: linear-gradient(90deg, var(--fill-color) 0%, var(--fill-color) var(--fill-percent), var(--track-color) var(--fill-percent), var(--track-color) 100%);
    }
    input[type=range]::-webkit-slider-thumb {
      -webkit-appearance: none;
      appearance: none;
      width: var(--thumb-size);
      height: var(--thumb-size);
      border-radius: 50%;
      background: #F3F4F6;
      border: 1px solid rgba(255, 255, 255, 0.4);
      margin-top: -4px;
    }
    input[type=range]::-moz-range-track {
      height: 6px;
      border-radius: 999px;
      background: var(--track-color);
    }
    input[type=range]::-moz-range-progress {
      height: 6px;
      border-radius: 999px;
      background: var(--fill-color);
    }
    input[type=range]::-moz-range-thumb {
      width: var(--thumb-size);
      height: var(--thumb-size);
      border-radius: 50%;
      background: #F3F4F6;
      border: 1px solid rgba(255, 255, 255, 0.4);
    }
    .player-controls select {
      background: rgba(22, 27, 51, 0.8);
      color: #fff;
      border: 1px solid rgba(255, 255, 255, 0.2);
      border-radius: 8px;
      padding: 6px 8px;
      font-size: 12px;
    }
    video { width: 100%; height: 100%; object-fit: contain; display: block; background: #000; }
    @media (max-width: 1024px) {
      .sidebar {
        position: fixed;
        left: 0;
        top: 0;
        transform: translateX(-100%);
        z-index: 99;
      }
      body.sidebar-open .sidebar {
        transform: translateX(0);
      }
      .sidebar-toggle {
        display: inline-flex;
        align-items: center;
        justify-content: center;
      }
      .main {
        padding: 20px;
      }
      .topbar {
        flex-wrap: wrap;
      }
    }
    @media (max-width: 640px) {
      .row { flex-direction: column; align-items: stretch; }
      input { min-width: 100%; }
      .hero { min-height: 220px; }
      .hero-title { font-size: 22px; }
      .grid { grid-template-columns: repeat(2, minmax(140px, 1fr)); }
    }
  </style>

</head>
<body>
  <div class="app">
    <aside class="sidebar" id="sidebar">
      <div class="brand">AceCinema</div>
      <div class="nav">
        <button id="navHome" class="active">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <path d="M3 10l9-7 9 7"/>
            <path d="M9 22V12h6v10"/>
          </svg>
          <span>Accueil</span>
        </button>
        <button id="navMovies">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <rect x="2" y="4" width="20" height="16" rx="2"/>
            <path d="M7 4v16M17 4v16M2 10h20"/>
          </svg>
          <span>Films</span>
        </button>
        <button id="navSeries">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <rect x="3" y="5" width="18" height="14" rx="2"/>
            <path d="M7 19l-2 3M17 19l2 3"/>
          </svg>
          <span>Series</span>
        </button>
        <button id="navList">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <path d="M8 6h13"/>
            <path d="M8 12h13"/>
            <path d="M8 18h13"/>
            <circle cx="3" cy="6" r="1"/>
            <circle cx="3" cy="12" r="1"/>
            <circle cx="3" cy="18" r="1"/>
          </svg>
          <span>Ma liste</span>
        </button>
        <button id="navSettings">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="12" cy="12" r="3"/>
            <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9c0 .66.39 1.26 1 1.51.31.13.66.2 1 .2H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>
          </svg>
          <span>Parametres</span>
        </button>
      </div>
    </aside>
    <main class="main">
      <div class="topbar">
        <div class="topbar-left">
          <button id="sidebarToggle" class="sidebar-toggle">&#9776;</button>
          <div class="search">
            <input id="searchInput" placeholder="Rechercher un film ou une serie"/>
          </div>
        </div>
        <div class="avatar-wrap hidden" id="avatarWrap">
          <button id="avatarBtn" class="avatar">A</button>
          <div id="avatarMenu" class="menu hidden">
            <button id="homeNav">Accueil</button>
            <button id="settingsNav">Parametres</button>
          </div>
        </div>
      </div>
      <div id="authPanel" class="panel auth-panel">
        <div class="row">
          <input id="email" placeholder="Email"/>
          <input id="password" placeholder="Mot de passe" type="password"/>
          <button id="loginBtn">Login</button>
        </div>
      </div>
      <div id="appShell" class="hidden">
        <div id="homeView" class="view">
          <div id="hero" class="hero">
            <div class="hero-content">
              <div id="heroMeta" class="hero-meta">A la une</div>
              <div id="heroTitle" class="hero-title">Bibliotheque AceCinema</div>
              <div id="heroDesc" class="hero-desc">Selection auto de la bibliotheque locale pour une lecture immediate.</div>
              <div class="hero-actions">
                <button id="heroPlay">Lecture</button>
                <button id="heroAdd" class="secondary">Ma liste</button>
              </div>
            </div>
          </div>
          <div class="section">
            <div class="section-header">
              <div class="section-title">Recemment ajoute</div>
            </div>
            <div id="recentRow" class="media-row"></div>
          </div>
          <div id="continueSection" class="section hidden">
            <div class="section-header">
              <div class="section-title">Continuer a regarder</div>
            </div>
            <div id="continueRow" class="media-row"></div>
          </div>
          <div class="section">
            <div class="section-header">
              <div class="section-title">Bibliotheque</div>
            </div>
            <div id="media" class="grid"></div>
          </div>
        </div>
        <div id="settingsView" class="view hidden">
          <div class="panel">
            <div class="row actions">
              <button id="refreshBtn" class="secondary">Refresh token</button>
              <button id="scanBtn">Scanner maintenant</button>
              <button id="logoutBtn" class="secondary">Logout</button>
            </div>
          </div>
          <div id="profilePanel" class="panel" style="margin-top:12px;">
            <div class="section-header">
              <div class="section-title">Mon compte</div>
            </div>
            <div class="row">
              <input id="profileUsername" placeholder="Nom d'utilisateur"/>
              <input id="profileEmail" placeholder="Email"/>
              <button id="profileSave">Enregistrer</button>
            </div>
            <div class="row" style="margin-top:8px;">
              <input id="oldPassword" type="password" placeholder="Ancien mot de passe"/>
              <input id="newPassword" type="password" placeholder="Nouveau mot de passe"/>
              <button id="changePasswordBtn" class="secondary">Changer le mot de passe</button>
            </div>
          </div>
          <div id="adminPanel" class="panel hidden" style="margin-top:12px;">
            <div class="row">
              <input id="libName" placeholder="Nom (optionnel)"/>
              <select id="libKind">
                <option value="movie">Films</option>
                <option value="series">Series</option>
              </select>
              <input id="libPath" placeholder="/mnt/media"/>
              <button id="addLibBtn">Ajouter source</button>
            </div>
            <div id="libList" class="grid"></div>
          </div>
          <div id="usersPanel" class="panel hidden" style="margin-top:12px;">
            <div class="section-header">
              <div class="section-title">Utilisateurs</div>
            </div>
            <div class="row">
              <input id="newUserEmail" placeholder="Email"/>
              <input id="newUserUsername" placeholder="Nom d'utilisateur"/>
              <input id="newUserPassword" type="password" placeholder="Mot de passe"/>
              <select id="newUserRole">
                <option value="user">User</option>
                <option value="admin">Admin</option>
              </select>
              <button id="createUserBtn">Ajouter</button>
            </div>
            <div id="userList" class="user-grid"></div>
          </div>
          <div class="panel" style="margin-top:12px;">
            <div class="row">
              <div>Avatar</div>
            </div>
            <div class="avatar-grid" id="avatarGrid"></div>
          </div>
        </div>
      </div>
    </main>
  </div>
  <div id="detailsOverlay" class="details-overlay hidden">
    <div class="details-modal">
      <div class="details-header">
        <div>
          <div id="detailsTitle" class="details-title"></div>
          <div id="detailsOriginal" class="details-original"></div>
          <div id="detailsMeta" class="details-meta"></div>
        </div>
        <button id="detailsClose" class="details-close">Fermer</button>
      </div>
      <div class="details-body">
        <img id="detailsPoster" class="details-poster" alt="Poster"/>
        <div>
          <div id="detailsTagline" class="details-tagline"></div>
          <div id="detailsSynopsis" class="details-synopsis"></div>
          <div class="details-actions">
            <button id="detailsPlay">Lecture</button>
            <button id="detailsAdd" class="secondary">Ma liste</button>
          </div>
          <div id="detailsGenres" class="details-chips"></div>
          <div class="details-grid">
            <div>
              <div class="label">Qualite video</div>
              <div id="detailsQuality" class="mono"></div>
            </div>
            <div>
              <div class="label">Pistes audio</div>
              <div id="detailsAudio" class="mono"></div>
            </div>
            <div>
              <div class="label">Realisateur</div>
              <div id="detailsDirector"></div>
            </div>
            <div>
              <div class="label">Scenario</div>
              <div id="detailsWriter"></div>
            </div>
            <div>
              <div class="label">Studios</div>
              <div id="detailsStudios"></div>
            </div>
          </div>
        </div>
      </div>
      <div>
        <div class="section-title">Casting</div>
        <div id="detailsCast" class="cast-row"></div>
      </div>
      <div>
        <div class="section-title">Dans le meme style</div>
        <div id="detailsSimilar" class="media-row"></div>
      </div>
    </div>
  </div>
  <div id="playerOverlay" class="player-overlay">
    <div class="player-shell">
      <div class="player-bar">
        <div id="playerTitle">Lecture</div>
        <button id="playerClose" class="player-close">Fermer</button>
      </div>
      <video id="playerVideo" playsinline></video>
      <div id="playerLoading" class="player-loading">
        <div class="spinner">
          <span style="--i:0"></span>
          <span style="--i:1"></span>
          <span style="--i:2"></span>
          <span style="--i:3"></span>
          <span style="--i:4"></span>
          <span style="--i:5"></span>
          <span style="--i:6"></span>
          <span style="--i:7"></span>
        </div>
        <div id="playerLoadingText" class="player-loading-text">Chargement...</div>
      </div>
      <div class="player-controls">
        <button id="playToggle" class="player-ctrl">&#9654;</button>
        <div id="timeLabel" class="time-label">0:00 / 0:00</div>
        <div class="seek-wrap">
          <div id="seekPreview" class="seek-preview"></div>
          <input id="seekBar" class="seek-bar" type="range" min="0" max="1000" step="0.1" value="0"/>
        </div>
        <input id="volumeBar" class="volume-bar" type="range" min="0" max="1" step="0.01" value="1"/>
        <select id="audioSelect">
          <option value="-1">Auto</option>
        </select>
        <button id="fullscreenBtn" class="player-ctrl">&#x26F6;</button>
        <div id="audioHint" style="color:#bbb;"></div>
      </div>
    </div>
  </div>
<script>
const DEBUG_SEEK = `+fmt.Sprintf("%t", debugSeek)+`;
let access = sessionStorage.getItem('access_token') || '';
let refreshToken = sessionStorage.getItem('refresh_token') || '';
const IDLE_TIMEOUT_MS = 60 * 60 * 1000;
const IDLE_CHECK_MS = 60 * 1000;
const TOKEN_REFRESH_MS = 25 * 60 * 1000;
let lastActivityAt = Date.now();
let idleTimer = null;
let refreshTimer = null;
const authPanel = document.getElementById('authPanel');
const appShell = document.getElementById('appShell');
const homeView = document.getElementById('homeView');
const settingsView = document.getElementById('settingsView');
const mediaGrid = document.getElementById('media');
const recentRow = document.getElementById('recentRow');
const continueSection = document.getElementById('continueSection');
const continueRow = document.getElementById('continueRow');
const hero = document.getElementById('hero');
const heroTitle = document.getElementById('heroTitle');
const heroDesc = document.getElementById('heroDesc');
const heroMeta = document.getElementById('heroMeta');
const heroPlay = document.getElementById('heroPlay');
const heroAdd = document.getElementById('heroAdd');
const detailsOverlay = document.getElementById('detailsOverlay');
const detailsTitle = document.getElementById('detailsTitle');
const detailsOriginal = document.getElementById('detailsOriginal');
const detailsMeta = document.getElementById('detailsMeta');
const detailsPoster = document.getElementById('detailsPoster');
const detailsTagline = document.getElementById('detailsTagline');
const detailsSynopsis = document.getElementById('detailsSynopsis');
const detailsPlay = document.getElementById('detailsPlay');
const detailsAdd = document.getElementById('detailsAdd');
const detailsGenres = document.getElementById('detailsGenres');
const detailsQuality = document.getElementById('detailsQuality');
const detailsAudio = document.getElementById('detailsAudio');
const detailsDirector = document.getElementById('detailsDirector');
const detailsWriter = document.getElementById('detailsWriter');
const detailsStudios = document.getElementById('detailsStudios');
const detailsCast = document.getElementById('detailsCast');
const detailsSimilar = document.getElementById('detailsSimilar');
const detailsClose = document.getElementById('detailsClose');
const adminPanel = document.getElementById('adminPanel');
const libList = document.getElementById('libList');
const scanBtn = document.getElementById('scanBtn');
const profileUsername = document.getElementById('profileUsername');
const profileEmail = document.getElementById('profileEmail');
const profileSave = document.getElementById('profileSave');
const oldPasswordInput = document.getElementById('oldPassword');
const newPasswordInput = document.getElementById('newPassword');
const changePasswordBtn = document.getElementById('changePasswordBtn');
const usersPanel = document.getElementById('usersPanel');
const userList = document.getElementById('userList');
const newUserEmail = document.getElementById('newUserEmail');
const newUserUsername = document.getElementById('newUserUsername');
const newUserPassword = document.getElementById('newUserPassword');
const newUserRole = document.getElementById('newUserRole');
const createUserBtn = document.getElementById('createUserBtn');
const avatarWrap = document.getElementById('avatarWrap');
const avatarBtn = document.getElementById('avatarBtn');
const avatarMenu = document.getElementById('avatarMenu');
const avatarGrid = document.getElementById('avatarGrid');
const searchInput = document.getElementById('searchInput');
const sidebarToggle = document.getElementById('sidebarToggle');
const sidebar = document.getElementById('sidebar');
const topbar = document.querySelector('.topbar');
const navHome = document.getElementById('navHome');
const navMovies = document.getElementById('navMovies');
const navSeries = document.getElementById('navSeries');
const navList = document.getElementById('navList');
const navSettings = document.getElementById('navSettings');
let mediaCache = [];
let continueItems = [];
let searchQuery = '';
let searchDebounce = null;
let heroItems = [];
let heroIndex = 0;
let heroTimer = null;
let activeCategory = 'all';
let currentUser = null;
function setAuthed(isAuthed) {
  authPanel.classList.toggle('hidden', isAuthed);
  appShell.classList.toggle('hidden', !isAuthed);
  avatarWrap.classList.toggle('hidden', !isAuthed);
  if (sidebar) {
    sidebar.classList.toggle('hidden', !isAuthed);
  }
  if (topbar) {
    topbar.classList.toggle('hidden', !isAuthed);
  }
  if (navList) {
    navList.classList.add('hidden');
  }
  if (heroAdd) {
    heroAdd.classList.add('hidden');
  }
  if (isAuthed) {
    startSessionTimers();
  } else {
    stopSessionTimers();
  }
  if (searchInput) {
    searchInput.disabled = !isAuthed;
    if (!isAuthed) {
      searchInput.value = '';
      searchQuery = '';
    }
  }
  if (!isAuthed && heroTimer) {
    clearInterval(heroTimer);
    heroTimer = null;
    heroItems = [];
    heroIndex = 0;
  }
  if (access) {
    console.log('token:', access);
  }
}
function showHome() {
  homeView.classList.remove('hidden');
  settingsView.classList.add('hidden');
  setActiveNav('home');
  document.body.classList.remove('sidebar-open');
}
function showSettings() {
  homeView.classList.add('hidden');
  settingsView.classList.remove('hidden');
  setActiveNav('settings');
  document.body.classList.remove('sidebar-open');
}
async function loadSettings() {
  await loadProfile();
  await loadLibraries();
}
function setStatus(msg, isError) {
  const level = isError ? 'error' : 'log';
  console[level]('status:', msg);
}
function setActiveNav(target) {
  const map = {
    home: navHome,
    movies: navMovies,
    series: navSeries,
    list: navList,
    settings: navSettings
  };
  Object.keys(map).forEach(key => {
    if (!map[key]) return;
    map[key].classList.toggle('active', key === target);
  });
}
function setAvatarLabel(name){
  const clean = (name || '').trim();
  const letter = clean ? clean[0].toUpperCase() : 'A';
  avatarBtn.textContent = letter;
}
async function loadProfile(){
  if (!access) { return; }
  const res = await fetch('/users/me', { headers: { Authorization: 'Bearer ' + access } });
  if (res.status === 401) {
    logout();
    setStatus('Session expiree, reconnecte-toi.', true);
    return;
  }
  if (!res.ok) {
    setStatus('Profile load failed: ' + res.status, true);
    return;
  }
  const user = await res.json();
  currentUser = user;
  profileUsername.value = user.username || '';
  profileEmail.value = user.email || '';
  setAvatarLabel(user.username || user.email);
}
async function saveProfile(){
  if (!access) { setStatus('Not logged in', true); return; }
  const email = profileEmail.value.trim();
  const username = profileUsername.value.trim();
  if (!email) {
    setStatus('Email requis', true);
    return;
  }
  const res = await fetch('/users/me', {
    method:'PUT',
    headers:{'Content-Type':'application/json', Authorization:'Bearer '+access},
    body: JSON.stringify({ email: email, username: username })
  });
  if (!res.ok) {
    setStatus('Profile update failed: ' + res.status, true);
    return;
  }
  setStatus('Profil mis a jour', false);
  await loadProfile();
}
async function changePassword(){
  if (!access) { setStatus('Not logged in', true); return; }
  const oldPass = oldPasswordInput.value.trim();
  const newPass = newPasswordInput.value.trim();
  if (!oldPass || !newPass) {
    setStatus('Ancien et nouveau mot de passe requis', true);
    return;
  }
  const res = await fetch('/auth/change-password', {
    method:'POST',
    headers:{'Content-Type':'application/json', Authorization:'Bearer '+access},
    body: JSON.stringify({ old_password: oldPass, new_password: newPass })
  });
  if (!res.ok) {
    let payload = null;
    try { payload = await res.json(); } catch (e) {}
    setStatus('Password update failed: ' + (payload && payload.error ? payload.error : res.status), true);
    return;
  }
  oldPasswordInput.value = '';
  newPasswordInput.value = '';
  setStatus('Mot de passe mis a jour', false);
}
function getDisplayTitle(m) {
  const meta = m.metadata || {};
  return meta.title || meta.original_title || meta.name || meta.original_name || m.title || 'Sans titre';
}
function getDisplayYear(m) {
  const meta = m.metadata || {};
  return m.year || meta.year || '';
}
function buildBadges(m) {
  const badges = [];
  const title = (m.title || '').toLowerCase();
  if (title.includes('2160') || title.includes('4k')) {
    badges.push('4K');
  }
  if (title.includes('hdr')) {
    badges.push('HDR');
  }
  return badges;
}
function parseJSONList(raw) {
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed : [];
  } catch (e) {
    return [];
  }
}
function formatRuntime(runtimeMin, durationSec) {
  let minutes = 0;
  if (runtimeMin) {
    minutes = parseInt(runtimeMin, 10);
  } else if (durationSec) {
    minutes = Math.round(durationSec / 60);
  }
  return minutes > 0 ? (minutes + ' min') : '';
}
function formatEndTime(durationSec) {
  if (!durationSec || !isFinite(durationSec)) return '';
  const end = new Date(Date.now() + durationSec * 1000);
  return end.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}
function formatRating(meta) {
  const rating = parseFloat(meta.rating || meta.vote_average || '');
  if (!isFinite(rating) || rating <= 0) return '';
  return rating.toFixed(1);
}
function parseFrameRate(value) {
  if (!value) return 0;
  if (typeof value === 'number') return value;
  if (value.includes('/')) {
    const parts = value.split('/');
    const num = parseFloat(parts[0]);
    const den = parseFloat(parts[1]);
    if (den) return num / den;
  }
  const direct = parseFloat(value);
  return isFinite(direct) ? direct : 0;
}
function formatVideoQuality(info) {
  if (!info || !info.video) return '';
  const video = info.video;
  const height = parseInt(video.height || 0, 10);
  const width = parseInt(video.width || 0, 10);
  const codec = (video.codec || '').toUpperCase();
  const fps = parseFrameRate(video.frame_rate);
  const bits = [];
  if (height) {
    bits.push(height + 'p');
  } else if (width && height) {
    bits.push(width + 'x' + height);
  }
  if (codec) bits.push(codec);
  if (fps) bits.push(fps.toFixed(2) + ' fps');
  return bits.join(' / ');
}
function formatAudioTracks(tracks) {
  if (!Array.isArray(tracks) || tracks.length === 0) return '';
  const labels = tracks.map(t => {
    const parts = [];
    if (t.language) parts.push(t.language.toUpperCase());
    if (t.codec) parts.push(t.codec.toUpperCase());
    if (t.channels) {
      const ch = parseInt(t.channels, 10);
      if (ch === 1) parts.push('1.0');
      else if (ch === 2) parts.push('2.0');
      else if (ch === 6) parts.push('5.1');
      else if (ch === 8) parts.push('7.1');
      else if (ch) parts.push(ch + 'ch');
    }
    return parts.filter(Boolean).join(' ');
  }).filter(Boolean);
  return labels.join(' / ');
}
function buildGenresChips(list) {
  detailsGenres.innerHTML = '';
  if (!list || list.length === 0) return;
  list.forEach(g => {
    const chip = document.createElement('span');
    chip.className = 'chip';
    chip.textContent = g;
    detailsGenres.appendChild(chip);
  });
}
function buildCastRow(cast) {
  detailsCast.innerHTML = '';
  if (!Array.isArray(cast) || cast.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'meta';
    empty.textContent = 'Casting indisponible';
    detailsCast.appendChild(empty);
    return;
  }
  cast.forEach(member => {
    const card = document.createElement('div');
    card.className = 'cast-card';
    const name = document.createElement('div');
    name.className = 'cast-name';
    name.textContent = member.name || '';
    const role = document.createElement('div');
    role.className = 'cast-role';
    role.textContent = member.character || '';
    card.appendChild(name);
    card.appendChild(role);
    detailsCast.appendChild(card);
  });
}
function buildSimilarRow(current) {
  detailsSimilar.innerHTML = '';
  if (!current) return;
  const currentGenres = new Set(parseJSONList((current.metadata || {}).genres_json).map(g => String(g).toLowerCase()));
  const candidates = mediaCache.filter(m => m.id !== current.id);
  let list = candidates;
  if (currentGenres.size > 0) {
    list = candidates.filter(m => {
      const genres = parseJSONList((m.metadata || {}).genres_json).map(g => String(g).toLowerCase());
      return genres.some(g => currentGenres.has(g));
    });
  }
  if (list.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'meta';
    empty.textContent = 'Aucun contenu similaire.';
    detailsSimilar.appendChild(empty);
    return;
  }
  list.slice(0, 12).forEach(m => {
    const card = createMediaCard(m);
    addRatingBadge(card, m);
    detailsSimilar.appendChild(card);
  });
}
async function openDetails(item) {
  if (!item) return;
  const meta = item.metadata || {};
  const titleText = getDisplayTitle(item);
  const originalTitle = meta.original_title || meta.original_name || '';
  const yearText = getDisplayYear(item);
  const baseRuntime = formatRuntime(meta.runtime, 0);
  const baseRating = formatRating(meta);
  const baseMetaParts = [];
  if (yearText) baseMetaParts.push(yearText);
  if (baseRuntime) baseMetaParts.push(baseRuntime);
  if (baseRating) baseMetaParts.push('TMDB ' + baseRating);
  detailsTitle.textContent = titleText;
  detailsOriginal.textContent = originalTitle && originalTitle !== titleText ? ('Titre original: ' + originalTitle) : '';
  detailsMeta.textContent = baseMetaParts.join(' | ');
  detailsPoster.src = item.poster_url || '';
  detailsPoster.alt = titleText;
  detailsTagline.textContent = meta.tagline ? ('"' + meta.tagline + '"') : '';
  detailsSynopsis.textContent = meta.plot || meta.overview || 'Synopsis indisponible.';
  detailsQuality.textContent = 'Analyse en cours...';
  detailsAudio.textContent = 'Analyse en cours...';
  detailsDirector.textContent = meta.director || '-';
  detailsWriter.textContent = meta.writer || '-';
  const studios = parseJSONList(meta.studios_json);
  detailsStudios.textContent = studios.length ? studios.join(', ') : '-';
  buildGenresChips(parseJSONList(meta.genres_json));
  buildCastRow(parseJSONList(meta.cast_json));
  buildSimilarRow(item);
  detailsPlay.onclick = () => {
    closeDetails();
    play(item.id, titleText, item.progress_ms || 0);
  };
  detailsAdd.onclick = () => setStatus('Ajoute a la liste (placeholder)', false);
  detailsOverlay.classList.remove('hidden');
  let durationSec = 0;
  try {
    const res = await fetch('/media/' + item.id + '/info', { headers: { Authorization: 'Bearer ' + access } });
    if (res.ok) {
      const info = await res.json();
      durationSec = parseFloat(info.duration || 0) || 0;
      detailsQuality.textContent = formatVideoQuality(info) || '-';
      const audioLabel = formatAudioTracks(info.audio_tracks || info.audio || []);
      detailsAudio.textContent = audioLabel || '-';
      const runtimeLabel = formatRuntime(meta.runtime, durationSec);
      const ratingLabel = formatRating(meta);
      const endLabel = formatEndTime(durationSec);
      const metaParts = [];
      if (yearText) metaParts.push(yearText);
      if (runtimeLabel) metaParts.push(runtimeLabel);
      if (ratingLabel) metaParts.push('TMDB ' + ratingLabel);
      if (endLabel) metaParts.push('Fin a ' + endLabel);
      detailsMeta.textContent = metaParts.join(' | ');
      return;
    }
  } catch (err) {
    console.error('details info failed', err);
  }
  const runtimeLabel = formatRuntime(meta.runtime, durationSec);
  const ratingLabel = formatRating(meta);
  const endLabel = formatEndTime(durationSec);
  const metaParts = [];
  if (yearText) metaParts.push(yearText);
  if (runtimeLabel) metaParts.push(runtimeLabel);
  if (ratingLabel) metaParts.push('TMDB ' + ratingLabel);
  if (endLabel) metaParts.push('Fin a ' + endLabel);
  detailsMeta.textContent = metaParts.join(' | ');
  detailsQuality.textContent = '-';
  detailsAudio.textContent = '-';
}
function closeDetails() {
  detailsOverlay.classList.add('hidden');
}
function getItemDurationSec(m) {
  const meta = m.metadata || {};
  const runtimeMin = parseFloat(meta.runtime || 0);
  if (isFinite(runtimeMin) && runtimeMin > 0) {
    return runtimeMin * 60;
  }
  const fromResponse = parseFloat(m.duration_sec || 0);
  if (isFinite(fromResponse) && fromResponse > 0) {
    return fromResponse;
  }
  const durationMs = parseFloat(m.duration_ms || 0);
  if (isFinite(durationMs) && durationMs > 0) {
    return durationMs / 1000;
  }
  const runtimeSec = parseFloat(meta.duration || 0);
  if (isFinite(runtimeSec) && runtimeSec > 0) {
    return runtimeSec;
  }
  return 0;
}
function addRatingBadge(card, item) {
  if (!card || !item) return;
  const rating = formatRating(item.metadata || {});
  if (!rating) return;
  const el = document.createElement('div');
  el.className = 'rating-badge';
  el.textContent = rating;
  card.appendChild(el);
}
function createMediaCard(m, options) {
  const el = document.createElement('div');
  el.className = 'card';
  const titleText = getDisplayTitle(m);
  const opts = options || {};
  if (m.poster_url) {
    const img = document.createElement('img');
    img.className = 'poster';
    img.loading = 'lazy';
    img.src = m.poster_url;
    img.alt = titleText;
    el.appendChild(img);
  }
  const badges = buildBadges(m);
  if (badges.length) {
    const badge = document.createElement('div');
    badge.className = 'badge';
    badge.textContent = badges.join(' ');
    el.appendChild(badge);
  }
  const title = document.createElement('div');
  title.className = 'card-title';
  title.textContent = titleText;
  const year = document.createElement('div');
  year.className = 'card-year';
  year.textContent = String(getDisplayYear(m) || '');
  if (m.progress_ms && m.progress_ms > 0) {
    const durationSec = getItemDurationSec(m);
    if (durationSec > 0) {
      const progressWrap = document.createElement('div');
      progressWrap.className = 'progress';
      const progressFill = document.createElement('span');
      const pct = Math.min(Math.max(m.progress_ms / 1000 / durationSec, 0), 1);
      progressFill.style.width = (pct * 100).toFixed(1) + '%';
      progressWrap.appendChild(progressFill);
      el.appendChild(progressWrap);
    }
  }
  const btn = document.createElement('button');
  btn.className = 'play-btn';
  btn.innerHTML = '&#9654;';
  const resumeMs = m.progress_ms || 0;
  btn.addEventListener('click', (e) => {
    e.stopPropagation();
    play(m.id, titleText, resumeMs);
  });
  el.appendChild(title);
  el.appendChild(year);
  el.appendChild(btn);
  if (opts.showRemove && resumeMs > 0) {
    const removeBtn = document.createElement('button');
    removeBtn.className = 'card-remove';
    removeBtn.innerHTML = '&times;';
    removeBtn.title = 'Retirer de Continuer';
    removeBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      removeProgress(m.id);
    });
    el.appendChild(removeBtn);
  }
  el.addEventListener('click', () => openDetails(m));
  return el;
}
function updateHero(item) {
  if (!item) {
    hero.style.setProperty('--hero-image', 'none');
    heroTitle.textContent = 'Bibliotheque AceCinema';
    heroDesc.textContent = 'Selection auto de la bibliotheque locale pour une lecture immediate.';
    heroMeta.textContent = 'A la une';
    heroPlay.disabled = true;
    hero.classList.remove('slide');
    return;
  }
  const titleText = getDisplayTitle(item);
  const yearText = getDisplayYear(item);
  const meta = item.metadata || {};
  const ratingLabel = formatRating(meta);
  const plot = meta.plot || meta.overview || '';
  const backdrop = item.backdrop_url || item.poster_url || '';
  const metaParts = [];
  if (yearText) metaParts.push(yearText);
  if (ratingLabel) metaParts.push('TMDB ' + ratingLabel);
  hero.style.setProperty('--hero-image', backdrop ? 'url(' + backdrop + ')' : 'none');
  heroTitle.textContent = titleText;
  heroDesc.textContent = plot || 'Lecture disponible en streaming direct.';
  heroMeta.textContent = metaParts.length ? metaParts.join(' | ') : 'A la une';
  heroPlay.disabled = false;
  heroPlay.onclick = () => play(item.id, titleText);
  hero.classList.remove('slide');
  void hero.offsetWidth;
  hero.classList.add('slide');
}
function shuffleList(list) {
  const arr = Array.isArray(list) ? list.slice() : [];
  for (let i = arr.length - 1; i > 0; i -= 1) {
    const j = Math.floor(Math.random() * (i + 1));
    const tmp = arr[i];
    arr[i] = arr[j];
    arr[j] = tmp;
  }
  return arr;
}
function startHeroCarousel(list) {
  if (heroTimer) {
    clearInterval(heroTimer);
    heroTimer = null;
  }
  heroItems = shuffleList(list).slice(0, 8);
  heroIndex = 0;
  if (heroItems.length === 0) {
    updateHero(null);
    return;
  }
  updateHero(heroItems[0]);
  if (heroItems.length < 2) {
    return;
  }
  heroTimer = setInterval(() => {
    heroIndex = (heroIndex + 1) % heroItems.length;
    hero.classList.add('fade');
    setTimeout(() => {
      updateHero(heroItems[heroIndex]);
      hero.classList.remove('fade');
    }, 350);
  }, 7000);
}
function renderHome(list, opts) {
  const updateHeroNow = !opts || opts.updateHero !== false;
  mediaGrid.innerHTML = '';
  recentRow.innerHTML = '';
  if (!Array.isArray(list) || list.length === 0) {
    if (updateHeroNow) {
      startHeroCarousel([]);
    }
    setStatus('Aucun media trouve.', false);
    return;
  }
  const sorted = list.slice().sort((a, b) => {
    const ta = Date.parse(a.created_at || '') || 0;
    const tb = Date.parse(b.created_at || '') || 0;
    return tb - ta;
  });
  if (updateHeroNow) {
    startHeroCarousel(sorted);
  }
  sorted.slice(0, 12).forEach(m => recentRow.appendChild(createMediaCard(m)));
  sorted.forEach(m => mediaGrid.appendChild(createMediaCard(m)));
}
function matchesQuery(item, q) {
  if (!q) return true;
  const meta = item.metadata || {};
  const hay = [
    item.title,
    meta.title,
    meta.original_title,
    meta.name,
    meta.original_name,
    meta.plot,
    meta.overview,
    String(item.year || meta.year || '')
  ].join(' ').toLowerCase();
  return hay.includes(q);
}
function matchesType(item) {
  const mType = (item.type || (item.metadata || {}).type || '').toLowerCase();
  if (activeCategory === 'movie') return mType === 'movie';
  if (activeCategory === 'series') return mType === 'series';
  return true;
}
function renderContinue(list) {
  continueRow.innerHTML = '';
  if (!Array.isArray(list) || list.length === 0) {
    continueSection.classList.add('hidden');
    return;
  }
  continueSection.classList.remove('hidden');
  list.forEach(m => continueRow.appendChild(createMediaCard(m, { showRemove: true })));
}
function applySearch() {
  const q = searchQuery.trim().toLowerCase();
  const filtered = mediaCache.filter(m => matchesQuery(m, q)).filter(matchesType);
  renderHome(filtered, { updateHero: false });
  const cont = continueItems
    .map(entry => Object.assign({}, entry.item || {}, {
      progress_ms: entry.position_ms || 0,
      duration_sec: entry.duration_sec || 0
    }))
    .filter(m => matchesQuery(m, q))
    .filter(matchesType);
  renderContinue(cont);
}
function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}
async function waitForManifest(url, sessionId, timeoutMs) {
  const deadline = Date.now() + (timeoutMs || 60000);
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url, { method: 'GET', cache: 'no-store' });
      if (res.ok) {
        return true;
      }
    } catch (err) {
      console.error('manifest check failed', err);
    }
    await sleep(1200);
  }
  if (sessionId && access) {
    try {
      const debugRes = await fetch('/admin/debug/hls/' + sessionId, { headers: { Authorization: 'Bearer ' + access } });
      if (debugRes.ok) {
        const dbg = await debugRes.json();
        console.error('hls debug log:', dbg.log || dbg);
      } else {
        console.error('hls debug failed:', debugRes.status);
      }
    } catch (err) {
      console.error('hls debug error', err);
    }
  }
  return false;
}
async function login() {
  const res = await fetch('/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({email:email.value,password:password.value})});
  const data = await res.json();
  access = data.access_token||'';
  refreshToken = data.refresh_token||'';
  sessionStorage.setItem('access_token', access);
  sessionStorage.setItem('refresh_token', refreshToken);
  setAuthed(res.ok);
  setStatus(res.ok ? 'Login OK' : 'Login failed', !res.ok);
  if (res.ok) {
    showHome();
    await loadProfile();
    await loadLibraries();
    loadMedia();
  }
}
async function refresh() {
  if (!refreshToken) { setStatus('No refresh token', true); return; }
  const res = await fetch('/auth/refresh',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({refresh_token:refreshToken})});
  const data = await res.json();
  access = data.access_token||'';
  refreshToken = data.refresh_token||refreshToken;
  if (access) {
    sessionStorage.setItem('access_token', access);
    console.log('token:', access);
  }
  if (data.refresh_token) {
    sessionStorage.setItem('refresh_token', data.refresh_token);
  }
  if (!res.ok) {
    logout();
    setStatus('Refresh failed, reconnecte-toi.', true);
    return;
  }
  setStatus('Refresh OK', false);
}
async function loadMedia() {
  if (!access) { setStatus('Not logged in', true); return; }
  setStatus('Chargement des medias...', false);
  const res = await fetch('/media',{headers:{Authorization:'Bearer '+access}});
  if (!res.ok) {
    if (res.status === 401) {
      logout();
      setStatus('Session expiree, reconnecte-toi.', true);
      return;
    }
    setStatus('Load failed: ' + res.status, true);
    return;
  }
  const list = await res.json();
  if (!Array.isArray(list)) {
    setStatus('Aucun media trouve.', false);
    return;
  }
  mediaCache = list;
  setStatus('Medias charges: ' + list.length, false);
  await loadContinue();
  applySearch();
}
async function loadContinue() {
  if (!access) { return; }
  try {
    const res = await fetch('/progress/continue', { headers: { Authorization: 'Bearer ' + access } });
    if (!res.ok) {
      continueItems = [];
      continueSection.classList.add('hidden');
      return;
    }
    const items = await res.json();
    if (!Array.isArray(items)) {
      continueItems = [];
      continueSection.classList.add('hidden');
      return;
    }
    continueItems = items;
    renderContinue(continueItems.map(entry => Object.assign({}, entry.item || {}, {
      progress_ms: entry.position_ms || 0,
      duration_sec: entry.duration_sec || 0
    })));
  } catch (err) {
    console.error('continue load failed', err);
    continueItems = [];
  }
}
async function removeProgress(mediaId) {
  if (!access || !mediaId) return;
  try {
    await fetch('/progress', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + access },
      body: JSON.stringify({ media_id: mediaId, position_ms: 0 })
    });
    continueItems = continueItems.filter(entry => entry.item && entry.item.id !== mediaId);
    renderContinue(continueItems.map(entry => Object.assign({}, entry.item || {}, {
      progress_ms: entry.position_ms || 0,
      duration_sec: entry.duration_sec || 0
    })));
  } catch (err) {
    console.error('remove progress failed', err);
  }
}
async function play(id, titleText, resumeMs){
  if (!access) { setStatus('Not logged in', true); return; }
  recordActivity();
  currentMediaId = id;
  catalogDuration = 0;
  hlsBaseOffset = 0;
  pendingSeekTime = null;
  lastProgressSentAt = 0;
  lastProgressSentMs = 0;
  openPlayer(titleText || 'Lecture');
  setPlayerLoading(true, 'Chargement...');
  await loadAudioTracks(id);
  const resumeSec = Math.max(0, (resumeMs || 0) / 1000);
  await startPlayback(id, resumeSec);
  await loadPlaybackInfo(id);
}
async function loadPlaybackInfo(mediaId) {
  if (!access) { return; }
  try {
    const res = await fetch('/media/' + mediaId + '/info', { headers: { Authorization: 'Bearer ' + access } });
    if (!res.ok) {
      return;
    }
    const info = await res.json();
    const duration = parseFloat(info.duration || 0);
    if (isFinite(duration) && duration > 0) {
      if (!catalogDuration || catalogDuration <= 0) {
        catalogDuration = duration;
      }
      if (currentStreamMode === 'hls') {
        seekBar.max = duration.toFixed(2);
        updateRangeFill(seekBar);
        timeLabel.textContent = formatTime(getGlobalTime()) + ' / ' + formatTime(duration);
        if (pendingSeekTime !== null) {
          requestSeek(pendingSeekTime);
        }
      }
    }
  } catch (err) {
    console.error('playback info failed', err);
  }
}
async function scanNow(){
  if (!access) { setStatus('Not logged in', true); return; }
  const res = await fetch('/admin/scan?reset=1',{method:'POST',headers:{Authorization:'Bearer '+access}});
  let payload = null;
  try { payload = await res.json(); } catch (e) {}
  if (!res.ok) {
    if (res.status === 401) {
      logout();
      setStatus('Session expiree, reconnecte-toi.', true);
      return;
    }
    if (res.status === 403) {
      setStatus('Acces refuse (admin requis).', true);
      return;
    }
    setStatus('Scan failed: ' + (payload && payload.error ? payload.error : res.status), true);
    return;
  }
  const added = payload && typeof payload.added === 'number' ? payload.added : 0;
  setStatus('Scan termine. Ajoutes: ' + added, false);
  loadMedia();
}
function logout(){
  access = '';
  refreshToken = '';
  sessionStorage.removeItem('access_token');
  sessionStorage.removeItem('refresh_token');
  setAuthed(false);
  mediaCache = [];
  continueItems = [];
  currentUser = null;
  renderHome([]);
  continueSection.classList.add('hidden');
  adminPanel.classList.add('hidden');
  usersPanel.classList.add('hidden');
  scanBtn.classList.add('hidden');
  avatarMenu.classList.add('hidden');
  profileUsername.value = '';
  profileEmail.value = '';
  oldPasswordInput.value = '';
  newPasswordInput.value = '';
  email.value = '';
  password.value = '';
  setAvatarLabel('A');
  closeDetails();
  showHome();
  setStatus('Deconnecte', false);
}
async function loadLibraries(){
  if (!access) { return; }
  const res = await fetch('/admin/libraries',{headers:{Authorization:'Bearer '+access}});
  if (res.status === 401) {
    logout();
    setStatus('Session expiree, reconnecte-toi.', true);
    return;
  }
  if (res.status === 403) {
    adminPanel.classList.add('hidden');
    scanBtn.classList.add('hidden');
    usersPanel.classList.add('hidden');
    return;
  }
  if (!res.ok) {
    setStatus('Admin load failed: ' + res.status, true);
    return;
  }
  scanBtn.classList.remove('hidden');
  const libs = await res.json();
  adminPanel.classList.remove('hidden');
  loadUsers();
  libList.innerHTML = '';
  libs.forEach(l => {
    const el = document.createElement('div');
    el.className = 'card';
    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = l.name || l.path;
    const meta = document.createElement('div');
    meta.className = 'meta';
    const kindLabel = (l.kind === 'series') ? 'Series' : 'Films';
    meta.textContent = kindLabel + ' | ' + l.path;
    const btn = document.createElement('button');
    btn.className = 'secondary';
    btn.textContent = 'Supprimer';
    btn.addEventListener('click', () => deleteLibrary(l.id));
    el.appendChild(title);
    el.appendChild(meta);
    el.appendChild(btn);
    libList.appendChild(el);
  });
}
async function addLibrary(){
  if (!access) { setStatus('Not logged in', true); return; }
  const name = document.getElementById('libName').value.trim();
  const kind = (document.getElementById('libKind').value || 'movie').trim();
  const path = document.getElementById('libPath').value.trim();
  if (!path) { setStatus('Chemin requis', true); return; }
  const res = await fetch('/admin/libraries',{
    method:'POST',
    headers:{'Content-Type':'application/json', Authorization:'Bearer '+access},
    body:JSON.stringify({name:name, path:path, kind: kind})
  });
  if (!res.ok) {
    setStatus('Add source failed: ' + res.status, true);
    return;
  }
  document.getElementById('libName').value = '';
  document.getElementById('libPath').value = '';
  await loadLibraries();
}
async function deleteLibrary(id){
  if (!access) { setStatus('Not logged in', true); return; }
  const res = await fetch('/admin/libraries/'+id,{method:'DELETE',headers:{Authorization:'Bearer '+access}});
  if (!res.ok) {
    setStatus('Delete failed: ' + res.status, true);
    return;
  }
  await loadLibraries();
}
async function loadUsers(){
  if (!access) { return; }
  const res = await fetch('/users', { headers: { Authorization: 'Bearer ' + access } });
  if (res.status === 401) {
    logout();
    setStatus('Session expiree, reconnecte-toi.', true);
    return;
  }
  if (res.status === 403) {
    usersPanel.classList.add('hidden');
    return;
  }
  if (!res.ok) {
    setStatus('Users load failed: ' + res.status, true);
    return;
  }
  const users = await res.json();
  usersPanel.classList.remove('hidden');
  renderUsers(users);
}
function renderUsers(users){
  userList.innerHTML = '';
  users.forEach(u => {
    const card = document.createElement('div');
    card.className = 'card user-card';
    const header = document.createElement('div');
    header.className = 'user-label';
    header.textContent = (u.role || 'user').toUpperCase();
    const row1 = document.createElement('div');
    row1.className = 'row';
    const email = document.createElement('input');
    email.value = u.email || '';
    email.placeholder = 'Email';
    const username = document.createElement('input');
    username.value = u.username || '';
    username.placeholder = "Nom d'utilisateur";
    row1.appendChild(username);
    row1.appendChild(email);
    const row2 = document.createElement('div');
    row2.className = 'row';
    const role = document.createElement('select');
    const optUser = document.createElement('option');
    optUser.value = 'user';
    optUser.textContent = 'User';
    const optAdmin = document.createElement('option');
    optAdmin.value = 'admin';
    optAdmin.textContent = 'Admin';
    role.appendChild(optUser);
    role.appendChild(optAdmin);
    role.value = u.role || 'user';
    const mustWrap = document.createElement('label');
    mustWrap.className = 'checkbox';
    const must = document.createElement('input');
    must.type = 'checkbox';
    must.checked = !!u.must_change;
    const mustText = document.createElement('span');
    mustText.textContent = 'Forcer changement';
    mustWrap.appendChild(must);
    mustWrap.appendChild(mustText);
    row2.appendChild(role);
    row2.appendChild(mustWrap);
    const row3 = document.createElement('div');
    row3.className = 'row';
    const password = document.createElement('input');
    password.type = 'password';
    password.placeholder = 'Nouveau mot de passe';
    const saveBtn = document.createElement('button');
    saveBtn.className = 'secondary';
    saveBtn.textContent = 'Sauver';
    saveBtn.addEventListener('click', () => {
      updateUser(u.id, {
        email: email.value.trim(),
        username: username.value.trim(),
        role: role.value,
        must_change: must.checked,
        password: password.value.trim()
      });
    });
    row3.appendChild(password);
    row3.appendChild(saveBtn);
    card.appendChild(header);
    card.appendChild(row1);
    card.appendChild(row2);
    card.appendChild(row3);
    userList.appendChild(card);
  });
}
async function createUser(){
  if (!access) { setStatus('Not logged in', true); return; }
  const email = newUserEmail.value.trim();
  const username = newUserUsername.value.trim();
  const password = newUserPassword.value.trim();
  const role = newUserRole.value || 'user';
  if (!email || !password) {
    setStatus('Email et mot de passe requis', true);
    return;
  }
  const res = await fetch('/users', {
    method:'POST',
    headers:{'Content-Type':'application/json', Authorization:'Bearer '+access},
    body: JSON.stringify({ email: email, username: username, password: password, role: role })
  });
  if (!res.ok) {
    let payload = null;
    try { payload = await res.json(); } catch (e) {}
    setStatus('Create user failed: ' + (payload && payload.error ? payload.error : res.status), true);
    return;
  }
  newUserEmail.value = '';
  newUserUsername.value = '';
  newUserPassword.value = '';
  await loadUsers();
}
async function updateUser(id, payload){
  if (!access) { setStatus('Not logged in', true); return; }
  if (!payload.email) {
    setStatus('Email requis', true);
    return;
  }
  const res = await fetch('/users/' + id, {
    method:'PUT',
    headers:{'Content-Type':'application/json', Authorization:'Bearer '+access},
    body: JSON.stringify(payload)
  });
  if (!res.ok) {
    let data = null;
    try { data = await res.json(); } catch (e) {}
    setStatus('Update user failed: ' + (data && data.error ? data.error : res.status), true);
    return;
  }
  await loadUsers();
}
const avatarColors = ['#6366F1', '#EC4899', '#10B981', '#0A0E27', '#161B33', '#9CA3AF'];
function isLightColor(hex) {
  const cleaned = hex.replace('#', '');
  if (cleaned.length !== 6) return false;
  const r = parseInt(cleaned.slice(0, 2), 16);
  const g = parseInt(cleaned.slice(2, 4), 16);
  const b = parseInt(cleaned.slice(4, 6), 16);
  const luminance = (0.299 * r + 0.587 * g + 0.114 * b) / 255;
  return luminance > 0.7;
}
function applyAvatar(color){
  const chosen = color || localStorage.getItem('avatar_color') || avatarColors[0];
  localStorage.setItem('avatar_color', chosen);
  avatarBtn.style.background = chosen;
  avatarBtn.style.color = isLightColor(chosen) ? '#0A0E27' : '#F3F4F6';
}
function buildAvatarPicker(){
  avatarGrid.innerHTML = '';
  avatarColors.forEach(color => {
    const el = document.createElement('div');
    el.className = 'avatar-option';
    el.style.background = color;
    el.addEventListener('click', () => applyAvatar(color));
    avatarGrid.appendChild(el);
  });
}
avatarBtn.addEventListener('click', () => {
  avatarMenu.classList.toggle('hidden');
});
document.getElementById('homeNav').addEventListener('click', () => {
  avatarMenu.classList.add('hidden');
  showHome();
  loadMedia();
});
document.getElementById('settingsNav').addEventListener('click', () => {
  avatarMenu.classList.add('hidden');
  showSettings();
  loadSettings();
});
document.addEventListener('click', (e) => {
  if (!avatarWrap.contains(e.target)) {
    avatarMenu.classList.add('hidden');
  }
});
const overlay = document.getElementById('playerOverlay');
const playerShell = document.querySelector('.player-shell');
const playerVideo = document.getElementById('playerVideo');
const playerTitle = document.getElementById('playerTitle');
const audioSelect = document.getElementById('audioSelect');
const audioHint = document.getElementById('audioHint');
const playToggle = document.getElementById('playToggle');
const seekBar = document.getElementById('seekBar');
const seekPreview = document.getElementById('seekPreview');
const playerLoading = document.getElementById('playerLoading');
const playerLoadingText = document.getElementById('playerLoadingText');
const volumeBar = document.getElementById('volumeBar');
const timeLabel = document.getElementById('timeLabel');
const fullscreenBtn = document.getElementById('fullscreenBtn');
const ICON_PLAY = '&#9654;';
const ICON_PAUSE = '&#10074;&#10074;';
let hls = null;
let currentMediaId = '';
let currentAudioIndex = -1;
let currentStreamMode = 'hls';
let currentStreamUrl = '';
let currentHlsSession = '';
let catalogDuration = 0;
let controlsTimer = null;
let isFullscreen = false;
let isSeeking = false;
let seekPointerActive = false;
let pendingSeekTime = null;
let hlsBaseOffset = 0;
let lastRestartAt = 0;
let restartTimer = null;
let queuedRestartTarget = null;
const RESTART_COOLDOWN_MS = 1200;
let lastHlsErrorAt = 0;
let hlsErrorCount = 0;
const HLS_ERROR_COOLDOWN_MS = 1500;
const HLS_ERROR_MAX = 5;
let seekHoldActive = false;
let seekHoldTarget = 0;
let lastProgressSentAt = 0;
let lastProgressSentMs = 0;
function debugSeekLog() {
  if (!DEBUG_SEEK) return;
  console.log.apply(console, arguments);
}
function recordActivity(){
  lastActivityAt = Date.now();
}
function startSessionTimers(){
  if (!idleTimer) {
    idleTimer = setInterval(() => {
      if (!access) return;
      if (Date.now() - lastActivityAt >= IDLE_TIMEOUT_MS) {
        setStatus('Session expiree (inactivite).', true);
        logout();
      }
    }, IDLE_CHECK_MS);
  }
  if (!refreshTimer) {
    refreshTimer = setInterval(() => {
      if (!access || !refreshToken) return;
      if (Date.now() - lastActivityAt >= IDLE_TIMEOUT_MS) return;
      refresh();
    }, TOKEN_REFRESH_MS);
  }
}
function stopSessionTimers(){
  if (idleTimer) {
    clearInterval(idleTimer);
    idleTimer = null;
  }
  if (refreshTimer) {
    clearInterval(refreshTimer);
    refreshTimer = null;
  }
}
function setPlayerLoading(active, text){
  if (!playerLoading) return;
  if (active) {
    playerLoading.classList.add('active');
    playerLoadingText.textContent = text || 'Chargement...';
  } else {
    playerLoading.classList.remove('active');
    playerLoadingText.textContent = '';
  }
}
function logSeekEvent(label, evt) {
  if (!DEBUG_SEEK) return;
  const clientX = evt && typeof evt.clientX === 'number' ? evt.clientX : null;
  debugSeekLog('[seek]', label, {
    clientX: clientX,
    value: seekBar.value,
    max: seekBar.max,
    isSeeking: isSeeking,
    seekPointerActive: seekPointerActive
  });
}
function logTimelineState(label) {
  if (!DEBUG_SEEK) return;
  const seekable = playerVideo.seekable && playerVideo.seekable.length
    ? [playerVideo.seekable.start(0), playerVideo.seekable.end(playerVideo.seekable.length - 1)]
    : [];
  const buffered = playerVideo.buffered && playerVideo.buffered.length
    ? [playerVideo.buffered.start(0), playerVideo.buffered.end(playerVideo.buffered.length - 1)]
    : [];
  const info = {
    label: label,
    duration: getDuration(),
    seekable: seekable,
    buffered: buffered,
    readyState: playerVideo.readyState,
    src: playerVideo.currentSrc || playerVideo.src || ''
  };
  debugSeekLog('[timeline]', info);
  if (hls) {
    debugSeekLog('[timeline] hls', {
      mediaDuration: hls.media ? hls.media.duration : null,
      levels: hls.levels ? hls.levels.length : 0
    });
  }
}
function setSeekHold(target) {
  seekHoldActive = true;
  seekHoldTarget = target;
  setPlayerLoading(true, 'Repositionnement...');
}
function releaseSeekHold() {
  seekHoldActive = false;
  setPlayerLoading(false);
}
function showSeekPreview(value, pct) {
  if (!seekPreview) return;
  seekPreview.textContent = formatTime(value);
  seekPreview.style.left = (pct * 100).toFixed(2) + '%';
  seekPreview.classList.add('visible');
}
function hideSeekPreview() {
  if (!seekPreview) return;
  seekPreview.classList.remove('visible');
}
function openPlayer(titleText){
  playerTitle.textContent = titleText;
  playerVideo.muted = false;
  playerVideo.volume = 1;
  volumeBar.value = '1';
  setTimeout(() => updateRangeFill(volumeBar), 0);
  playToggle.innerHTML = ICON_PLAY;
  seekBar.value = '0';
  updateRangeFill(seekBar);
  timeLabel.textContent = '0:00 / 0:00';
  overlay.style.display = 'flex';
  document.body.classList.add('no-scroll');
  showControls();
}
function closePlayer(){
  maybeSendProgress(true);
  if (hls) {
    hls.destroy();
    hls = null;
  }
  playerVideo.pause();
  playerVideo.removeAttribute('src');
  playerVideo.load();
  currentMediaId = '';
  currentStreamMode = 'hls';
  currentStreamUrl = '';
  currentHlsSession = '';
  catalogDuration = 0;
  hlsBaseOffset = 0;
  pendingSeekTime = null;
  lastRestartAt = 0;
  queuedRestartTarget = null;
  seekHoldActive = false;
  seekHoldTarget = 0;
  if (restartTimer) {
    clearTimeout(restartTimer);
    restartTimer = null;
  }
  if (controlsTimer) {
    clearTimeout(controlsTimer);
    controlsTimer = null;
  }
  setPlayerLoading(false);
  overlay.style.display = 'none';
  document.body.classList.remove('no-scroll');
}
function showControls(){
  playerShell.classList.add('controls-visible');
  if (controlsTimer) {
    clearTimeout(controlsTimer);
  }
  controlsTimer = setTimeout(() => {
    playerShell.classList.remove('controls-visible');
  }, 3000);
}
playerVideo.addEventListener('timeupdate', () => {
  if (currentStreamMode === 'hls' && !seekHoldActive) {
    hlsErrorCount = 0;
  }
  recordActivity();
});
playerShell.addEventListener('mousemove', showControls);
async function loadAudioTracks(mediaId){
  audioSelect.innerHTML = '<option value="-1">Auto</option>';
  audioHint.textContent = '';
  const res = await fetch('/media/'+mediaId+'/tracks',{headers:{Authorization:'Bearer '+access}});
  if (!res.ok) {
    audioHint.textContent = 'Pistes audio non detectees';
    return;
  }
  const tracks = await res.json();
  if (!Array.isArray(tracks) || tracks.length === 0) {
    audioHint.textContent = 'Pistes audio non detectees';
    return;
  }
  tracks.forEach(t => {
    const label = (t.language || '') + (t.title ? (' - ' + t.title) : '') + (t.codec ? (' [' + t.codec + ']') : '');
    const opt = document.createElement('option');
    opt.value = String(t.audio_index);
    opt.textContent = label.trim() || ('Track ' + t.index);
    audioSelect.appendChild(opt);
  });
  const fr = tracks.find(t => (t.language || '').toLowerCase().startsWith('fr'));
  currentAudioIndex = fr ? fr.audio_index : tracks[0].audio_index;
  audioSelect.value = String(currentAudioIndex);
}
async function startPlayback(mediaId, startAt){
  hlsBaseOffset = 0;
  pendingSeekTime = null;
  currentStreamUrl = '';
  currentHlsSession = '';
  setPlayerLoading(true, 'Chargement...');
  const startValue = (isFinite(startAt) && startAt > 0) ? startAt : 0;
  const audioIndex = isFinite(currentAudioIndex) ? currentAudioIndex : -1;
  const url = '/stream/session?mediaId=' + encodeURIComponent(mediaId) + '&audio=' + encodeURIComponent(audioIndex) + (startValue > 0 ? ('&start=' + encodeURIComponent(startValue.toFixed(2))) : '');
  const res = await fetch(url,{headers:{Authorization:'Bearer '+access}});
  const bodyText = await res.text();
  let data = null;
  try { data = JSON.parse(bodyText); } catch (e) {}
  if (!res.ok) {
    if (data && data.log) {
      console.error('ffmpeg log:', data.log);
    }
    const message = data && data.error ? data.error : (bodyText || res.status);
    setStatus('Stream failed: ' + message, true);
    setPlayerLoading(false);
    return;
  }
  if (!data || !data.url) {
    setStatus('Stream url missing', true);
    setPlayerLoading(false);
    return;
  }
  currentStreamMode = data.mode || 'hls';
  currentStreamUrl = data.url;
  currentHlsSession = data.session || '';
  const duration = parseFloat(data.duration || 0);
  if (isFinite(duration) && duration > 0) {
    catalogDuration = duration;
    seekBar.max = duration.toFixed(2);
    updateRangeFill(seekBar);
  }
  if (currentStreamMode === 'direct') {
    audioSelect.disabled = true;
    await startDirect(data.url, startValue);
    return;
  }
  audioSelect.disabled = false;
  await startTranscode(data.url, data.session, startValue, data.status);
}

async function startDirect(url, startAt){
  hlsBaseOffset = 0;
  pendingSeekTime = null;
  if (hls) {
    hls.destroy();
    hls = null;
  }
  playerVideo.pause();
  playerVideo.removeAttribute('src');
  playerVideo.load();
  const startValue = (isFinite(startAt) && startAt > 0) ? startAt : 0;
  if (startValue > 0) {
    pendingSeekTime = startValue;
  }
  playerVideo.src = url;
  playerVideo.addEventListener('canplay', () => {
    releaseSeekHold();
    setPlayerLoading(false);
    playerVideo.play().catch(err => console.error('play failed', err));
  }, { once: true });
  logTimelineState('start');
}

async function startTranscode(url, sessionId, startAt, status){
  hlsBaseOffset = 0;
  pendingSeekTime = null;
  currentHlsSession = sessionId || '';
  let didAutoPlay = false;
  const tryAutoPlay = () => {
    if (didAutoPlay) return;
    const buffered = playerVideo.buffered;
    if (!buffered || buffered.length === 0) return;
    const end = buffered.end(buffered.length - 1);
    const ahead = end - playerVideo.currentTime;
    if (ahead >= 4) {
      didAutoPlay = true;
      releaseSeekHold();
      setPlayerLoading(false);
      playerVideo.play().catch(err => console.error('play failed', err));
    }
  };
  if (hls) {
    hls.destroy();
    hls = null;
  }
  playerVideo.pause();
  playerVideo.removeAttribute('src');
  playerVideo.load();
  const startValue = (isFinite(startAt) && startAt > 0) ? startAt : 0;
  if (startValue > 0) {
    hlsBaseOffset = startValue;
  }
  if (catalogDuration > 0) {
    seekBar.max = catalogDuration.toFixed(2);
    updateRangeFill(seekBar);
  }
  if (startValue > 0 && catalogDuration > 0) {
    timeLabel.textContent = formatTime(startValue) + ' / ' + formatTime(catalogDuration);
  }
  if (status === 'starting') {
    setStatus('Transcode en demarrage...', false);
    setPlayerLoading(true, 'Transcodage...');
  }
  const ready = await waitForManifest(url, sessionId, 90000);
  if (!ready) {
    setStatus('HLS manifest not ready', true);
    setPlayerLoading(false);
    return;
  }
  if (window.Hls && Hls.isSupported()) {
    hls = new Hls({
      maxBufferLength: 30,
      maxMaxBufferLength: 60,
      backBufferLength: 15,
      maxBufferSize: 60 * 1000 * 1000
    });
    hls.on(Hls.Events.FRAG_BUFFERED, () => {
      tryAutoPlay();
    });
    hls.on(Hls.Events.LEVEL_LOADED, (event, data) => {
      if (!data || !data.details) return;
      if (hlsBaseOffset === 0 && data.details.fragments && data.details.fragments.length > 0) {
        const base = parseFloat(data.details.fragments[0].start || 0);
        if (isFinite(base) && base >= 0) {
          hlsBaseOffset = base;
        }
      }
      if (catalogDuration > 0) {
        seekBar.max = catalogDuration.toFixed(2);
        updateRangeFill(seekBar);
        if (pendingSeekTime !== null) {
          requestSeek(pendingSeekTime);
        }
      }
    });
    if (DEBUG_SEEK) {
      hls.on(Hls.Events.FRAG_CHANGED, (event, data) => {
        const frag = data && data.frag ? data.frag : null;
        debugSeekLog('[hls] FRAG_CHANGED', frag ? { sn: frag.sn, start: frag.start, duration: frag.duration } : data);
      });
      hls.on(Hls.Events.FRAG_LOADED, (event, data) => {
        const frag = data && data.frag ? data.frag : null;
        debugSeekLog('[hls] FRAG_LOADED', frag ? { sn: frag.sn, start: frag.start, duration: frag.duration } : data);
      });
    }
    hls.on(Hls.Events.ERROR, (event, data) => {
      console.error('hls error', data);
      const code = data && data.response ? data.response.code : 0;
      const details = data && data.details ? data.details : '';
      if (details === 'bufferFullError' && hls) {
        try {
          hls.stopLoad();
          hls.recoverMediaError();
          hls.startLoad(playerVideo.currentTime || 0);
        } catch (e) {}
        return;
      }
      const isServerErr = code >= 500;
      const isNet = data && data.type === Hls.ErrorTypes.NETWORK_ERROR;
      const isMedia = data && data.type === Hls.ErrorTypes.MEDIA_ERROR;
      if (isServerErr || isNet || isMedia) {
        recoverFromHlsError(details || data.type || code);
      }
      if (data && data.fatal) {
        setStatus('HLS failed: ' + (details || data.type), true);
      }
    });
    hls.loadSource(url);
    hls.attachMedia(playerVideo);
    playerVideo.addEventListener('canplay', () => {
      releaseSeekHold();
      setPlayerLoading(false);
      tryAutoPlay();
    }, { once: true });
    logTimelineState('start');
  } else {
    playerVideo.src = url;
    playerVideo.addEventListener('canplay', () => {
      releaseSeekHold();
      setPlayerLoading(false);
      tryAutoPlay();
    }, { once: true });
    logTimelineState('start');
  }
}
audioSelect.addEventListener('change', async () => {
  const val = parseInt(audioSelect.value, 10);
  currentAudioIndex = isNaN(val) ? -1 : val;
  if (currentStreamMode === 'hls' && currentMediaId) {
    const pos = getGlobalTime();
    await startPlayback(currentMediaId, pos);
  }
});
function formatTime(seconds){
  if (!isFinite(seconds)) { return '0:00'; }
  const s = Math.floor(seconds % 60);
  const m = Math.floor((seconds / 60) % 60);
  const h = Math.floor(seconds / 3600);
  const mm = (m < 10 ? '0' + m : m);
  const ss = (s < 10 ? '0' + s : s);
  if (h > 0) {
    return h + ':' + mm + ':' + ss;
  }
  return m + ':' + ss;
}
function updateRangeFill(range){
  const min = parseFloat(range.min || '0');
  const max = parseFloat(range.max || '100');
  const val = parseFloat(range.value || '0');
  const pct = max > min ? ((val - min) / (max - min)) : 0;
  const pctClamped = Math.min(Math.max(pct, 0), 1);
  range.style.setProperty('--fill-percent', (pctClamped * 100).toFixed(2) + '%');
}
async function sendProgress(posMs, force){
  if (!currentMediaId || !access) return;
  try {
    await fetch('/progress', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + access },
      body: JSON.stringify({ media_id: currentMediaId, position_ms: posMs }),
      keepalive: !!force
    });
  } catch (err) {
    console.error('progress update failed', err);
  }
}
function maybeSendProgress(force){
  if (!currentMediaId || !access) return;
  let posMs = Math.floor(getGlobalTime() * 1000);
  if (!isFinite(posMs) || posMs < 1000) return;
  const durationSec = getDuration();
  if (isFinite(durationSec) && durationSec > 0) {
    const durationMs = durationSec * 1000;
    if (posMs >= durationMs * 0.95) {
      posMs = 0;
    }
  }
  const now = Date.now();
  if (!force) {
    if (now - lastProgressSentAt < 10000) return;
    if (Math.abs(posMs - lastProgressSentMs) < 5000) return;
  }
  lastProgressSentAt = now;
  lastProgressSentMs = posMs;
  sendProgress(posMs, force);
}
function getDuration(){
  if (currentStreamMode === 'hls' && catalogDuration > 0) {
    return catalogDuration;
  }
  if (isFinite(playerVideo.duration) && playerVideo.duration > 0) {
    return playerVideo.duration;
  }
  if (playerVideo.seekable && playerVideo.seekable.length > 0) {
    return playerVideo.seekable.end(playerVideo.seekable.length - 1);
  }
  if (catalogDuration > 0) {
    return catalogDuration;
  }
  const fallback = parseFloat(seekBar.max || '0');
  if (isFinite(fallback) && fallback > 0) {
    return fallback;
  }
  return 0;
}
function getGlobalTime(){
  if (currentStreamMode === 'hls') {
    const base = hlsBaseOffset || 0;
    return playerVideo.currentTime + base;
  }
  return playerVideo.currentTime;
}
function requestSeek(target){
  const duration = getDuration();
  if (!duration || !isFinite(duration)) {
    pendingSeekTime = target;
    return;
  }
  const clamped = Math.min(Math.max(target, 0), duration);
  const local = hlsBaseOffset ? Math.max(0, clamped - hlsBaseOffset) : clamped;
  if (DEBUG_SEEK) {
    debugSeekLog('[seek] requestSeek', {
      target: target,
      duration: duration,
      hlsBaseOffset: hlsBaseOffset,
      clamped: clamped,
      local: local,
      before: playerVideo.currentTime
    });
  }
  playerVideo.currentTime = local;
  if (DEBUG_SEEK) {
    debugSeekLog('[seek] requestSeek after', {
      after: playerVideo.currentTime
    });
    setTimeout(() => {
      debugSeekLog('[vid] +200ms currentTime', playerVideo.currentTime);
    }, 200);
  }
  pendingSeekTime = null;
}
function getBufferedRanges() {
  const ranges = [];
  const buffered = playerVideo.buffered;
  if (!buffered) return ranges;
  for (let i = 0; i < buffered.length; i += 1) {
    const start = buffered.start(i) + (hlsBaseOffset || 0);
    const end = buffered.end(i) + (hlsBaseOffset || 0);
    ranges.push({ start: start, end: end });
  }
  return ranges;
}
function canSeekLocally(target) {
  if (currentStreamMode !== 'hls') {
    return true;
  }
  const ranges = getBufferedRanges();
  if (!ranges.length) return false;
  return ranges.some(r => target >= (r.start + 0.2) && target <= (r.end - 0.2));
}
async function handleSeekTarget(target, allowRestart) {
  if (!isFinite(target)) return;
  if (currentStreamMode === 'direct') {
    releaseSeekHold();
    requestSeek(target);
    return;
  }
  if (canSeekLocally(target)) {
    if (DEBUG_SEEK) {
      debugSeekLog('[seek] local', { target: target, offset: hlsBaseOffset });
    }
    releaseSeekHold();
    requestSeek(target);
    return;
  }
  if (allowRestart && currentMediaId) {
    if (DEBUG_SEEK) {
      debugSeekLog('[seek] restart', { target: target, offset: hlsBaseOffset });
    }
    setStatus('Repositionnement...', false);
    setSeekHold(target);
    scheduleRestart(target);
    return;
  }
  pendingSeekTime = target;
}
function scheduleRestart(target) {
  if (!currentMediaId) return;
  const now = Date.now();
  const elapsed = now - lastRestartAt;
  if (elapsed >= RESTART_COOLDOWN_MS) {
    lastRestartAt = now;
    startPlayback(currentMediaId, target);
    return;
  }
  queuedRestartTarget = target;
  if (restartTimer) return;
  restartTimer = setTimeout(() => {
    restartTimer = null;
    if (queuedRestartTarget !== null) {
      const t = queuedRestartTarget;
      queuedRestartTarget = null;
      lastRestartAt = Date.now();
      startPlayback(currentMediaId, t);
    }
  }, Math.max(RESTART_COOLDOWN_MS - elapsed, 0));
}
function recoverFromHlsError(reason) {
  if (currentStreamMode !== 'hls' || !currentMediaId) return;
  if (hls) {
    try { hls.stopLoad(); } catch (e) {}
    try { hls.detachMedia(); } catch (e) {}
  }
  const now = Date.now();
  if (now - lastHlsErrorAt < HLS_ERROR_COOLDOWN_MS) return;
  lastHlsErrorAt = now;
  hlsErrorCount += 1;
  if (hlsErrorCount > HLS_ERROR_MAX) {
    setStatus('Erreur HLS persistante. Reessaie dans quelques secondes.', true);
    return;
  }
  const resumeAt = Math.max(getGlobalTime() - 0.5, 0);
  setStatus('Lecture interrompue, reprise en cours...', true);
  setPlayerLoading(true, 'Reconnexion...');
  playerVideo.pause();
  setSeekHold(resumeAt);
  scheduleRestart(resumeAt);
  if (DEBUG_SEEK) {
    debugSeekLog('[hls] recover', { reason: reason, resumeAt: resumeAt });
  }
}
function seekFromPointer(evt, preview){
  const duration = getDuration();
  if (!duration) return 0;
  const rect = seekBar.getBoundingClientRect();
  const pct = rect.width ? (evt.clientX - rect.left) / rect.width : 0;
  const clamped = Math.min(Math.max(pct, 0), 1);
  const value = duration * clamped;
  seekBar.value = value.toFixed(2);
  updateRangeFill(seekBar);
  timeLabel.textContent = formatTime(value) + ' / ' + formatTime(duration);
  if (preview) {
    showSeekPreview(value, clamped);
  }
  return value;
}
  playerVideo.addEventListener('loadedmetadata', () => {
    const duration = getDuration();
    if (!duration) { return; }
    seekBar.max = duration.toFixed(2);
    updateRangeFill(seekBar);
    timeLabel.textContent = formatTime(getGlobalTime()) + ' / ' + formatTime(duration);
    if (pendingSeekTime !== null) {
      requestSeek(pendingSeekTime);
    }
  });
  playerVideo.addEventListener('timeupdate', () => {
    if (seekHoldActive) {
      const duration = getDuration();
      if (!duration) { return; }
      const pos = Math.min(Math.max(seekHoldTarget, 0), duration);
      seekBar.max = duration.toFixed(2);
      seekBar.value = pos.toFixed(2);
      updateRangeFill(seekBar);
      timeLabel.textContent = formatTime(pos) + ' / ' + formatTime(duration);
      return;
    }
    if (isSeeking) return;
    const duration = getDuration();
    if (!duration) { return; }
    const pos = Math.min(Math.max(getGlobalTime(), 0), duration);
    seekBar.max = duration.toFixed(2);
    seekBar.value = pos.toFixed(2);
    updateRangeFill(seekBar);
    timeLabel.textContent = formatTime(pos) + ' / ' + formatTime(duration);
    maybeSendProgress(false);
  });
  if (DEBUG_SEEK) {
    playerVideo.addEventListener('seeking', () => debugSeekLog('[vid] seeking', playerVideo.currentTime));
    playerVideo.addEventListener('seeked', () => debugSeekLog('[vid] seeked', playerVideo.currentTime));
    playerVideo.addEventListener('timeupdate', () => debugSeekLog('[vid] timeupdate', playerVideo.currentTime));
    playerVideo.addEventListener('waiting', () => debugSeekLog('[vid] waiting', playerVideo.currentTime));
    playerVideo.addEventListener('stalled', () => debugSeekLog('[vid] stalled', playerVideo.currentTime));
    playerVideo.addEventListener('error', () => debugSeekLog('[vid] error', playerVideo.error));
  }
seekBar.addEventListener('input', () => {
  isSeeking = true;
  logSeekEvent('input', null);
  const duration = getDuration();
  if (!duration) { return; }
  const pos = Math.min(Math.max(parseFloat(seekBar.value), 0), duration);
  timeLabel.textContent = formatTime(pos) + ' / ' + formatTime(duration);
  updateRangeFill(seekBar);
});
seekBar.addEventListener('change', () => {
  const t = parseFloat(seekBar.value);
  logSeekEvent('change', null);
  hideSeekPreview();
  handleSeekTarget(t, true);
  isSeeking = false;
});
seekBar.addEventListener('pointerdown', (e) => {
  if (e.button !== 0) return;
  seekPointerActive = true;
  isSeeking = true;
  seekBar.setPointerCapture(e.pointerId);
  logSeekEvent('pointerdown', e);
  seekFromPointer(e, true);
});
seekBar.addEventListener('pointermove', (e) => {
  if (!seekPointerActive) return;
  logSeekEvent('pointermove', e);
  seekFromPointer(e, true);
});
seekBar.addEventListener('pointerup', (e) => {
  if (!seekPointerActive) return;
  logSeekEvent('pointerup', e);
  const value = seekFromPointer(e, true);
  handleSeekTarget(value, true);
  if (seekBar.hasPointerCapture(e.pointerId)) {
    seekBar.releasePointerCapture(e.pointerId);
  }
  seekPointerActive = false;
  isSeeking = false;
  setTimeout(hideSeekPreview, 200);
});
seekBar.addEventListener('pointercancel', (e) => {
  if (seekBar.hasPointerCapture(e.pointerId)) {
    seekBar.releasePointerCapture(e.pointerId);
  }
  seekPointerActive = false;
  isSeeking = false;
  hideSeekPreview();
});
seekBar.addEventListener('click', (e) => {
  logSeekEvent('click', e);
  const value = seekFromPointer(e, true);
  handleSeekTarget(value, true);
  logTimelineState('click');
  setTimeout(hideSeekPreview, 500);
});
volumeBar.addEventListener('input', () => {
  let v = parseFloat(volumeBar.value);
  if (!isFinite(v)) {
    v = 1;
    volumeBar.value = '1';
  }
  playerVideo.volume = v;
  updateRangeFill(volumeBar);
});
playToggle.addEventListener('click', () => {
  if (playerVideo.paused) {
    playerVideo.play().catch(err => console.error('play failed', err));
  } else {
    playerVideo.pause();
  }
});
playerVideo.addEventListener('play', () => { playToggle.innerHTML = ICON_PAUSE; });
playerVideo.addEventListener('pause', () => {
  playToggle.innerHTML = ICON_PLAY;
  maybeSendProgress(true);
});
playerVideo.addEventListener('ended', () => {
  maybeSendProgress(true);
});
fullscreenBtn.addEventListener('click', () => {
  if (document.fullscreenElement) {
    document.exitFullscreen();
  } else if (playerShell && playerShell.requestFullscreen) {
    playerShell.requestFullscreen();
  }
});
document.addEventListener('fullscreenchange', () => {
  isFullscreen = !!document.fullscreenElement;
  playerShell.classList.toggle('is-fullscreen', isFullscreen);
  if (isFullscreen) {
    showControls();
  } else {
    playerShell.classList.add('controls-visible');
  }
});
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden') {
    maybeSendProgress(true);
  }
});
window.addEventListener('beforeunload', () => {
  maybeSendProgress(true);
});
document.getElementById('playerClose').addEventListener('click', closePlayer);
overlay.addEventListener('click', (e) => {
  if (e.target === overlay) closePlayer();
});
if (detailsClose) {
  detailsClose.addEventListener('click', closeDetails);
}
if (detailsOverlay) {
  detailsOverlay.addEventListener('click', (e) => {
    if (e.target === detailsOverlay) closeDetails();
  });
}
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    if (overlay.style.display === 'flex') closePlayer();
    if (!detailsOverlay.classList.contains('hidden')) closeDetails();
  }
});
if (searchInput) {
  searchInput.addEventListener('input', (e) => {
    searchQuery = e.target.value || '';
    if (searchDebounce) {
      clearTimeout(searchDebounce);
    }
    searchDebounce = setTimeout(() => {
      applySearch();
    }, 300);
  });
}
if (sidebarToggle) {
  sidebarToggle.addEventListener('click', () => {
    document.body.classList.toggle('sidebar-open');
  });
}
if (navHome) navHome.addEventListener('click', () => {
  activeCategory = 'all';
  showHome();
  loadMedia();
});
if (navMovies) navMovies.addEventListener('click', () => {
  activeCategory = 'movie';
  showHome();
  setActiveNav('movies');
  loadMedia();
});
if (navSeries) navSeries.addEventListener('click', () => {
  activeCategory = 'series';
  showHome();
  setActiveNav('series');
  loadMedia();
});
if (navList) navList.addEventListener('click', () => {
  activeCategory = 'all';
  showHome();
  setActiveNav('list');
  loadMedia();
});
if (navSettings) navSettings.addEventListener('click', () => { showSettings(); loadSettings(); });
if (heroAdd) {
  heroAdd.addEventListener('click', () => {
    setStatus('Ajoute a la liste (placeholder)', false);
  });
}
document.getElementById('loginBtn').addEventListener('click', login);
document.getElementById('refreshBtn').addEventListener('click', refresh);
document.getElementById('logoutBtn').addEventListener('click', logout);
document.getElementById('scanBtn').addEventListener('click', scanNow);
document.getElementById('addLibBtn').addEventListener('click', addLibrary);
profileSave.addEventListener('click', saveProfile);
changePasswordBtn.addEventListener('click', changePassword);
createUserBtn.addEventListener('click', createUser);
['click','keydown','mousemove','scroll','touchstart'].forEach(evt => {
  document.addEventListener(evt, recordActivity, { passive: true });
});
playerVideo.addEventListener('play', recordActivity);
window.addEventListener('pagehide', () => {
  sessionStorage.removeItem('access_token');
  sessionStorage.removeItem('refresh_token');
  access = '';
  refreshToken = '';
});
window.addEventListener('resize', () => {
  updateRangeFill(seekBar);
  updateRangeFill(volumeBar);
});
setAuthed(!!access);
if (access) {
  applyAvatar();
  buildAvatarPicker();
  showHome();
  loadProfile().then(() => loadLibraries()).then(loadMedia);
} else {
  applyAvatar();
  buildAvatarPicker();
}
</script>
</body>
</html>`)
}
