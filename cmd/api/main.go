package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"crypto/rand"
	"encoding/hex"

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

var buildVersion = envDefault("BUILD_VERSION", "dev")

type hlsSession struct {
	id         string
	path       string
	audioIndex int
	dir        string
	logPath    string
	cmd        *exec.Cmd
	exitErr   error
	done      chan struct{}
	lastAccess time.Time
}

type hlsManager struct {
	baseDir  string
	mu       sync.Mutex
	sessions map[string]*hlsSession
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
		r.Use(authSvc.RequireRole("admin"))
		r.Post("/", handleCreateUser(session, cfg.Keyspace))
	})

	r.Route("/media", func(r chi.Router) {
		r.Use(authSvc.RequireAuth)
		r.Get("/", handleListMedia(mediaSvc))
		r.Get("/{id}", handleGetMedia(mediaSvc))
		r.Get("/{id}/assets", handleGetAssets(mediaSvc))
		r.Get("/{id}/tracks", handleAudioTracks(mediaSvc, session, cfg.Keyspace, cfg.MediaRoot))
	})

	r.With(authSvc.RequireAuth).Put("/progress", handleUpdateProgress(mediaSvc))

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
	r.With(authSvc.RequireAuth).Get("/stream/hls", handleStreamHLS(hlsMgr, session, cfg.Keyspace, cfg.MediaRoot))
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
			Password string `json:"password"`
			Role     string `json:"role"`
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
		if err := db.CreateUser(r.Context(), session, keyspace, req.Email, req.Password, req.Role, false); err != nil {
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
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid body")
			return
		}
		req.Path = strings.TrimSpace(req.Path)
		if req.Path == "" {
			errorJSON(w, http.StatusBadRequest, "path required")
			return
		}
		if req.Name == "" {
			req.Name = filepath.Base(req.Path)
		}
		lib, err := db.CreateLibrary(r.Context(), session, keyspace, req.Name, req.Path)
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
		sess, err := mgr.Create(full, audioIndex)
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
		sess.lastAccess = time.Now()
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
	roots, err := loadLibraryRoots(ctx, session, keyspace, fallback)
	if err != nil {
		return 0, err
	}
	if len(roots) == 0 {
		return 0, nil
	}
	return svc.ScanRoots(ctx, roots, refreshExisting)
}

func loadLibraryRoots(ctx context.Context, session *gocql.Session, keyspace, fallback string) ([]string, error) {
	libs, err := db.ListLibraries(ctx, session, keyspace)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(libs))
	for _, lib := range libs {
		if strings.TrimSpace(lib.Path) != "" {
			roots = append(roots, lib.Path)
		}
	}
	if len(roots) == 0 && fallback != "" {
		roots = append(roots, fallback)
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

func newHlsManager() *hlsManager {
	base := "/tmp/acecinema-hls"
	_ = os.MkdirAll(base, 0o755)
	m := &hlsManager{
		baseDir:  base,
		sessions: make(map[string]*hlsSession),
	}
	go m.cleanupLoop()
	return m
}

func (m *hlsManager) Create(path string, audioIndex int) (*hlsSession, error) {
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
		path:       path,
		audioIndex: audioIndex,
		dir:        dir,
		logPath:    filepath.Join(dir, "ffmpeg.log"),
		done:       make(chan struct{}),
		lastAccess: time.Now(),
	}
	m.mu.Lock()
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
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.cleanupOld(30 * time.Minute)
	}
}

func (m *hlsManager) cleanupOld(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, sess := range m.sessions {
		if now.Sub(sess.lastAccess) > maxAge {
			if sess.cmd != nil && sess.cmd.Process != nil {
				_ = sess.cmd.Process.Kill()
			}
			_ = os.RemoveAll(sess.dir)
			delete(m.sessions, id)
		}
	}
}

func (m *hlsManager) startSession(sess *hlsSession) {
	log.Printf("hls start: path=%s audio=%d dir=%s", sess.path, sess.audioIndex, sess.dir)
	mapAudio := "0:a:0?"
	if sess.audioIndex >= 0 {
		mapAudio = fmt.Sprintf("0:a:%d?", sess.audioIndex)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-analyzeduration", "20M",
		"-probesize", "20M",
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
		"-c:a", "aac",
		"-ac", "2",
		"-b:a", "160k",
		"-sn",
		"-f", "hls",
		"-hls_time", "4",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(sess.dir, "seg%03d.ts"),
		filepath.Join(sess.dir, "index.m3u8"),
	}
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
	fmt.Fprint(w, `<!doctype html>
<html>
<head>
  <meta charset="utf-8"/>
  <title>AceCinema</title>
  <script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.12"></script>
  <style>
    :root {
      --snow: #fffbfe;
      --grey: #7a7d7d;
      --dust-grey: #d0cfcf;
      --charcoal: #565254;
      --white: #ffffff;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      color: var(--charcoal);
      background: radial-gradient(1200px 600px at 20% 10%, var(--snow), var(--white)) fixed;
      font-family: "Garamond", "Palatino Linotype", "Book Antiqua", serif;
      font-size: 16.5px;
    }
    .page {
      max-width: 1100px;
      margin: 0 auto;
      padding: 28px 20px 80px;
    }
    h1 {
      margin: 0 0 8px;
      font-size: 34px;
      letter-spacing: 0.5px;
    }
    .subtitle {
      color: var(--grey);
      margin-bottom: 18px;
    }
    .panel {
      background: var(--white);
      border: 1px solid var(--dust-grey);
      border-radius: 14px;
      padding: 16px;
      box-shadow: 0 10px 30px rgba(86, 82, 84, 0.08);
    }
    .hidden { display: none; }
    .topbar {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 16px;
      gap: 10px;
    }
    .logo {
      font-size: 34px;
      letter-spacing: 0.5px;
      margin: 0;
    }
    .avatar-wrap { position: relative; }
    .avatar {
      width: 40px;
      height: 40px;
      border-radius: 50%;
      border: 1px solid var(--dust-grey);
      background: var(--charcoal);
      color: var(--white);
      font-weight: bold;
    }
    .menu {
      position: absolute;
      right: 0;
      top: 48px;
      background: var(--white);
      border: 1px solid var(--dust-grey);
      border-radius: 10px;
      min-width: 160px;
      box-shadow: 0 10px 24px rgba(86, 82, 84, 0.18);
      padding: 6px;
      z-index: 50;
    }
    .menu button {
      width: 100%;
      text-align: left;
      background: var(--white);
      color: var(--charcoal);
      border: none;
      padding: 10px;
    }
    .menu button:hover {
      background: var(--snow);
    }
    .view { margin-top: 12px; }
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
      border: 1px solid var(--dust-grey);
      cursor: pointer;
    }
    .row {
      display: flex;
      flex-wrap: wrap;
      gap: 10px;
      align-items: center;
    }
    input, button {
      border-radius: 10px;
      padding: 10px 12px;
      border: 1px solid var(--dust-grey);
      font-size: 14px;
    }
    input { min-width: 220px; }
    button {
      background: var(--charcoal);
      color: var(--white);
      cursor: pointer;
      transition: transform 0.08s ease, box-shadow 0.2s ease;
    }
    button.secondary {
      background: var(--white);
      color: var(--charcoal);
    }
    button:hover { transform: translateY(-1px); box-shadow: 0 6px 18px rgba(86, 82, 84, 0.18); }
    .actions { margin-top: 12px; }
    .token {
      margin-top: 8px;
      font-family: "Consolas", "Courier New", monospace;
      font-size: 12px;
      color: var(--grey);
      word-break: break-all;
    }
    .status {
      margin-top: 8px;
      padding: 8px 10px;
      border-radius: 10px;
      background: var(--snow);
      border: 1px dashed var(--dust-grey);
      min-height: 38px;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
      gap: 12px;
      margin-top: 16px;
    }
    .card {
      border: 1px solid var(--dust-grey);
      border-radius: 14px;
      padding: 12px;
      background: var(--white);
    }
    .card-title { font-weight: bold; margin-top: 10px; }
    .card-year { color: var(--grey); font-size: 12px; margin-top: 4px; }
    .poster {
      width: 100%;
      aspect-ratio: 2 / 3;
      background: var(--dust-grey);
      border-radius: 10px;
      object-fit: cover;
    }
    .play-btn {
      margin-top: 10px;
      width: 36px;
      height: 36px;
      border-radius: 50%;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0;
    }
    .meta { color: var(--grey); font-size: 12px; }
    .player-overlay {
      position: fixed;
      inset: 0;
      background: #000;
      display: none;
      align-items: center;
      justify-content: center;
      z-index: 999;
      padding: 20px;
    }
    .player-shell {
      width: min(960px, 100%);
      background: #000;
      border-radius: 16px;
      overflow: hidden;
      border: 1px solid #333;
      position: relative;
      display: flex;
      flex-direction: column;
      max-height: 90vh;
    }
    .player-controls {
      display: flex;
      gap: 10px;
      align-items: center;
      padding: 8px 12px;
      background: #161616;
      color: #fff;
      font-size: 12px;
      border-top: 1px solid #2a2a2a;
      position: absolute;
      left: 0;
      right: 0;
      bottom: 0;
      opacity: 0;
      pointer-events: none;
      transition: opacity 0.2s ease;
    }
    .player-shell.controls-visible .player-controls,
    .player-shell.controls-visible .player-bar {
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
    .player-ctrl {
      background: #262626;
      color: #fff;
      border: 1px solid #3a3a3a;
      border-radius: 8px;
      padding: 6px 10px;
      cursor: pointer;
    }
    .player-ctrl:active,
    .player-close:active {
      transform: translateY(1px);
    }
    .seek-bar { flex: 1; }
    .volume-bar { width: 110px; }
    .time-label { min-width: 90px; color: #cfcfcf; }
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
      --track-color: #2f2f2f;
      --fill-color: #000;
    }
    input[type=range]::-webkit-slider-runnable-track {
      height: 6px;
      border-radius: 999px;
      background: linear-gradient(90deg, var(--fill-color) 0, var(--fill-color) var(--fill-px, 0px), var(--track-color) var(--fill-px, 0px), var(--track-color) 100%);
    }
    input[type=range]::-webkit-slider-thumb {
      -webkit-appearance: none;
      appearance: none;
      width: var(--thumb-size);
      height: var(--thumb-size);
      border-radius: 50%;
      background: #d0cfcf;
      border: 1px solid #3a3a3a;
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
      background: #d0cfcf;
      border: 1px solid #3a3a3a;
    }
    .player-controls select {
      background: #1f1f1f;
      color: #fff;
      border: 1px solid #2f2f2f;
      border-radius: 8px;
      padding: 6px 8px;
      font-size: 12px;
    }
    .player-bar {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding: 8px 12px;
      background: #000;
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
    }
    .player-close {
      background: #2b2b2b;
      color: #fff;
      border: 1px solid #3a3a3a;
      cursor: pointer;
    }
    .player-controls {
      z-index: 2;
    }
    video { width: 100%; height: auto; max-height: 90vh; object-fit: contain; display: block; background: #000; }
    @media (max-width: 640px) {
      .row { flex-direction: column; align-items: stretch; }
      input { min-width: 100%; }
    }
  </style>
</head>
<body>
  <div class="page">
    <div class="topbar">
      <div>
        <div class="logo">AceCinema</div>
      </div>
      <div class="avatar-wrap hidden" id="avatarWrap">
        <button id="avatarBtn" class="avatar">A</button>
        <div id="avatarMenu" class="menu hidden">
          <button id="homeNav">Accueil</button>
          <button id="settingsNav">Parametres</button>
        </div>
      </div>
    </div>
    <div id="authPanel" class="panel">
      <div class="row">
        <input id="email" placeholder="email" value="admin@example.com"/>
        <input id="password" placeholder="password" type="password" value="changeme-admin"/>
        <button id="loginBtn">Login</button>
      </div>
    </div>
    <div id="appShell" class="hidden">
      <div id="homeView" class="view">
        <div id="media" class="grid"></div>
      </div>
      <div id="settingsView" class="view hidden">
        <div class="panel">
          <div class="row actions">
            <button id="refreshBtn" class="secondary">Refresh token</button>
            <button id="scanBtn">Scanner maintenant</button>
            <button id="logoutBtn" class="secondary">Logout</button>
          </div>
        </div>
        <div id="adminPanel" class="panel hidden" style="margin-top:12px;">
          <div class="row">
            <input id="libName" placeholder="Nom (optionnel)"/>
            <input id="libPath" placeholder="/mnt/media"/>
            <button id="addLibBtn">Ajouter source</button>
          </div>
          <div id="libList" class="grid"></div>
        </div>
        <div class="panel" style="margin-top:12px;">
          <div class="row">
            <div>Avatar</div>
          </div>
          <div class="avatar-grid" id="avatarGrid"></div>
        </div>
      </div>
    </div>
    <div style="margin-top:20px; color: var(--grey); font-size: 12px;">Build: `+buildVersion+`</div>
  </div>
  <div id="playerOverlay" class="player-overlay">
    <div class="player-shell">
      <div class="player-bar">
        <div id="playerTitle">Lecture</div>
        <button id="playerClose" class="player-close">Fermer</button>
      </div>
      <video id="playerVideo" playsinline></video>
      <div class="player-controls">
        <button id="playToggle" class="player-ctrl">&#9654;</button>
        <div id="timeLabel" class="time-label">0:00 / 0:00</div>
        <input id="seekBar" class="seek-bar" type="range" min="0" max="1000" step="0.1" value="0"/>
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
let access = localStorage.getItem('access_token') || '';
let refreshToken = localStorage.getItem('refresh_token') || '';
const authPanel = document.getElementById('authPanel');
const appShell = document.getElementById('appShell');
const homeView = document.getElementById('homeView');
const settingsView = document.getElementById('settingsView');
const mediaGrid = document.getElementById('media');
const adminPanel = document.getElementById('adminPanel');
const libList = document.getElementById('libList');
const scanBtn = document.getElementById('scanBtn');
const avatarWrap = document.getElementById('avatarWrap');
const avatarBtn = document.getElementById('avatarBtn');
const avatarMenu = document.getElementById('avatarMenu');
const avatarGrid = document.getElementById('avatarGrid');
function setAuthed(isAuthed) {
  authPanel.classList.toggle('hidden', isAuthed);
  appShell.classList.toggle('hidden', !isAuthed);
  avatarWrap.classList.toggle('hidden', !isAuthed);
  if (access) {
    console.log('token:', access);
  }
}
function showHome() {
  homeView.classList.remove('hidden');
  settingsView.classList.add('hidden');
}
function showSettings() {
  homeView.classList.add('hidden');
  settingsView.classList.remove('hidden');
}
function setStatus(msg, isError) {
  const level = isError ? 'error' : 'log';
  console[level]('status:', msg);
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
  localStorage.setItem('access_token', access);
  localStorage.setItem('refresh_token', refreshToken);
  setAuthed(res.ok);
  setStatus(res.ok ? 'Login OK' : 'Login failed', !res.ok);
  if (res.ok) {
    showHome();
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
    localStorage.setItem('access_token', access);
    console.log('token:', access);
  }
  if (data.refresh_token) {
    localStorage.setItem('refresh_token', data.refresh_token);
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
  mediaGrid.innerHTML='';
  if (!Array.isArray(list) || list.length === 0) {
    setStatus('Aucun media trouve.', false);
    return;
  }
  setStatus('Medias charges: ' + list.length, false);
  list.forEach(m=>{
    const el = document.createElement('div');
    el.className = 'card';
    if (m.poster_url) {
      const img = document.createElement('img');
      img.className = 'poster';
      img.loading = 'lazy';
      img.src = m.poster_url;
      const meta = m.metadata || {};
      const metaTitle = meta.title || meta.original_title || meta.name || meta.original_name || '';
      const displayTitle = metaTitle || m.title || 'Sans titre';
      img.alt = displayTitle;
      el.appendChild(img);
    }
    const title = document.createElement('div');
    title.className = 'card-title';
    const meta = m.metadata || {};
    const metaTitle = meta.title || meta.original_title || meta.name || meta.original_name || '';
    const metaYear = meta.year || '';
    const displayTitle = metaTitle || m.title || 'Sans titre';
    title.textContent = displayTitle;
    const year = document.createElement('div');
    year.className = 'card-year';
    year.textContent = m.year ? String(m.year) : (metaYear || '');
    const btn = document.createElement('button');
    btn.className = 'play-btn';
    btn.innerHTML = '&#9654;';
    btn.addEventListener('click', () => play(m.id, displayTitle));
    el.appendChild(title);
    el.appendChild(year);
    el.appendChild(btn);
    mediaGrid.appendChild(el);
  });
}
async function play(id, titleText){
  if (!access) { setStatus('Not logged in', true); return; }
  const res = await fetch('/media/'+id+'/assets',{headers:{Authorization:'Bearer '+access}});
  if (!res.ok) {
    if (res.status === 401) {
      logout();
      setStatus('Session expiree, reconnecte-toi.', true);
      return;
    }
    setStatus('Assets load failed: ' + res.status, true);
    return;
  }
  const assets = await res.json();
  if(assets.length===0){setStatus('No assets for media', true); return;}
  currentAssetPath = assets[0].path;
  currentMediaId = id;
  openPlayer(titleText || 'Lecture');
  await loadAudioTracks(id);
  await startHls(currentAssetPath, currentAudioIndex);
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
  localStorage.removeItem('access_token');
  localStorage.removeItem('refresh_token');
  setAuthed(false);
  mediaGrid.innerHTML = '';
  adminPanel.classList.add('hidden');
  scanBtn.classList.add('hidden');
  avatarMenu.classList.add('hidden');
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
    return;
  }
  if (!res.ok) {
    setStatus('Admin load failed: ' + res.status, true);
    return;
  }
  scanBtn.classList.remove('hidden');
  const libs = await res.json();
  adminPanel.classList.remove('hidden');
  libList.innerHTML = '';
  libs.forEach(l => {
    const el = document.createElement('div');
    el.className = 'card';
    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = l.name || l.path;
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.textContent = l.path;
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
  const path = document.getElementById('libPath').value.trim();
  if (!path) { setStatus('Chemin requis', true); return; }
  const res = await fetch('/admin/libraries',{
    method:'POST',
    headers:{'Content-Type':'application/json', Authorization:'Bearer '+access},
    body:JSON.stringify({name:name, path:path})
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
const avatarColors = ['#565254', '#7a7d7d', '#d0cfcf', '#fffbfe', '#3a3738', '#8a8587'];
function applyAvatar(color){
  const chosen = color || localStorage.getItem('avatar_color') || avatarColors[0];
  localStorage.setItem('avatar_color', chosen);
  avatarBtn.style.background = chosen;
  avatarBtn.style.color = chosen === '#fffbfe' ? '#565254' : '#ffffff';
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
  loadLibraries();
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
const volumeBar = document.getElementById('volumeBar');
const timeLabel = document.getElementById('timeLabel');
const fullscreenBtn = document.getElementById('fullscreenBtn');
const ICON_PLAY = '&#9654;';
const ICON_PAUSE = '&#10074;&#10074;';
let hls = null;
let currentAssetPath = '';
let currentMediaId = '';
let currentAudioIndex = -1;
let currentHlsSession = '';
let currentHlsUrl = '';
let hlsDuration = 0;
let controlsTimer = null;
let isFullscreen = false;
function openPlayer(titleText){
  playerTitle.textContent = titleText;
  playerVideo.muted = false;
  playerVideo.volume = 1;
  volumeBar.value = '1';
  updateRangeFill(volumeBar);
  playToggle.innerHTML = ICON_PLAY;
  seekBar.value = '0';
  updateRangeFill(seekBar);
  timeLabel.textContent = '0:00 / 0:00';
  overlay.style.display = 'flex';
  showControls();
}
function closePlayer(){
  if (hls) {
    hls.destroy();
    hls = null;
  }
  playerVideo.pause();
  playerVideo.removeAttribute('src');
  playerVideo.load();
  currentAssetPath = '';
  currentMediaId = '';
  currentHlsSession = '';
  currentHlsUrl = '';
  hlsDuration = 0;
  if (controlsTimer) {
    clearTimeout(controlsTimer);
    controlsTimer = null;
  }
  overlay.style.display = 'none';
}
function showControls(){
  playerShell.classList.add('controls-visible');
  if (!isFullscreen) {
    return;
  }
  if (controlsTimer) {
    clearTimeout(controlsTimer);
  }
  controlsTimer = setTimeout(() => {
    playerShell.classList.remove('controls-visible');
  }, 2500);
}
overlay.addEventListener('mousemove', showControls);
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
async function startHls(path, audioIndex){
  hlsDuration = 0;
  currentHlsSession = '';
  currentHlsUrl = '';
  if (hls) {
    hls.destroy();
    hls = null;
  }
  playerVideo.pause();
  playerVideo.removeAttribute('src');
  playerVideo.load();
  const url = '/stream/hls?path='+encodeURIComponent(path)+'&audio='+encodeURIComponent(audioIndex);
  const res = await fetch(url,{headers:{Authorization:'Bearer '+access}});
  const bodyText = await res.text();
  let data = null;
  try { data = JSON.parse(bodyText); } catch (e) {}
  if (!res.ok) {
    if (data && data.log) {
      console.error('hls ffmpeg log:', data.log);
    }
    if (data && data.session) {
      currentHlsSession = data.session;
    }
    const message = data && data.error ? data.error : (bodyText || res.status);
    setStatus('HLS failed: ' + message, true);
    return;
  }
  if (!data || !data.url) {
    setStatus('HLS url missing', true);
    return;
  }
  currentHlsSession = data.session || '';
  currentHlsUrl = data.url;
  if (data.status === 'starting') {
    setStatus('HLS en demarrage...', false);
  }
  if (data.duration) {
    const d = parseFloat(data.duration);
    if (!isNaN(d)) {
      hlsDuration = d;
      seekBar.max = d.toFixed(2);
      updateRangeFill(seekBar);
    }
  }
  const ready = await waitForManifest(data.url, data.session, 90000);
  if (!ready) {
    setStatus('HLS manifest not ready', true);
    return;
  }
  if (window.Hls && Hls.isSupported()) {
    hls = new Hls({
      maxBufferLength: 180,
      maxMaxBufferLength: 360,
      backBufferLength: 90,
      maxBufferSize: 256 * 1000 * 1000
    });
    hls.on(Hls.Events.ERROR, (event, data) => {
      console.error('hls error', data);
      if (data && data.fatal) {
        setStatus('HLS failed: ' + (data.details || data.type), true);
      }
    });
    hls.loadSource(data.url);
    hls.attachMedia(playerVideo);
  } else {
    playerVideo.src = data.url;
  }
  playerVideo.play().catch(err => console.error('play failed', err));
}
audioSelect.addEventListener('change', async () => {
  const val = parseInt(audioSelect.value, 10);
  currentAudioIndex = isNaN(val) ? -1 : val;
  if (currentAssetPath) {
    await startHls(currentAssetPath, currentAudioIndex);
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
  const width = range.getBoundingClientRect().width || 1;
  const thumbSize = parseFloat(getComputedStyle(range).getPropertyValue('--thumb-size')) || 14;
  const usable = Math.max(width - thumbSize, 1);
  const fillPx = (usable * pctClamped) + (thumbSize / 2);
  range.style.setProperty('--fill', (pctClamped * 100).toFixed(2) + '%');
  range.style.setProperty('--fill-px', fillPx.toFixed(2) + 'px');
}
function getDuration(){
  if (hlsDuration > 0) {
    return hlsDuration;
  }
  if (isFinite(playerVideo.duration) && playerVideo.duration > 0) {
    return playerVideo.duration;
  }
  if (playerVideo.seekable && playerVideo.seekable.length > 0) {
    return playerVideo.seekable.end(playerVideo.seekable.length - 1);
  }
  return 0;
}
  playerVideo.addEventListener('loadedmetadata', () => {
    const duration = getDuration();
    if (!duration) { return; }
    seekBar.max = duration.toFixed(2);
    updateRangeFill(seekBar);
    timeLabel.textContent = formatTime(playerVideo.currentTime) + ' / ' + formatTime(duration);
  });
  playerVideo.addEventListener('timeupdate', () => {
    const duration = getDuration();
    if (!duration) { return; }
    const pos = Math.min(Math.max(playerVideo.currentTime, 0), duration);
    seekBar.max = duration.toFixed(2);
    seekBar.value = pos.toFixed(2);
    updateRangeFill(seekBar);
    timeLabel.textContent = formatTime(pos) + ' / ' + formatTime(duration);
  });
seekBar.addEventListener('input', () => {
  const duration = getDuration();
  if (!duration) { return; }
  const pos = Math.min(Math.max(parseFloat(seekBar.value), 0), duration);
  timeLabel.textContent = formatTime(pos) + ' / ' + formatTime(duration);
  updateRangeFill(seekBar);
});
seekBar.addEventListener('change', () => {
  const duration = getDuration();
  if (!duration) { return; }
  const t = parseFloat(seekBar.value);
  const global = Math.min(Math.max(t, 0), duration);
  playerVideo.currentTime = global;
});
volumeBar.addEventListener('input', () => {
  let v = parseFloat(volumeBar.value);
  if (v > 0.98) {
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
playerVideo.addEventListener('pause', () => { playToggle.innerHTML = ICON_PLAY; });
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
document.getElementById('playerClose').addEventListener('click', closePlayer);
overlay.addEventListener('click', (e) => {
  if (e.target === overlay) closePlayer();
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && overlay.style.display === 'flex') closePlayer();
});
document.getElementById('loginBtn').addEventListener('click', login);
document.getElementById('refreshBtn').addEventListener('click', refresh);
document.getElementById('logoutBtn').addEventListener('click', logout);
document.getElementById('scanBtn').addEventListener('click', scanNow);
document.getElementById('addLibBtn').addEventListener('click', addLibrary);
window.addEventListener('resize', () => {
  updateRangeFill(seekBar);
  updateRangeFill(volumeBar);
});
setAuthed(!!access);
if (access) {
  applyAvatar();
  buildAvatarPicker();
  showHome();
  loadLibraries().then(loadMedia);
} else {
  applyAvatar();
  buildAvatarPicker();
}
</script>
</body>
</html>`)
}
