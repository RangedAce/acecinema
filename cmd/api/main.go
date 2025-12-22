package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	ScyllaHosts []string
	ScyllaPort  int
	Keyspace    string
	Consistency string
	Replication int
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
	mediaSvc := media.NewService(session, cfg.Keyspace, cfg.MediaRoot)

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
	})

	r.With(authSvc.RequireAuth).Put("/progress", handleUpdateProgress(mediaSvc))

	r.Route("/admin", func(r chi.Router) {
		r.Use(authSvc.RequireRole("admin"))
		r.Get("/libraries", handleListLibraries(session, cfg.Keyspace))
		r.Post("/libraries", handleCreateLibrary(session, cfg.Keyspace))
		r.Delete("/libraries/{id}", handleDeleteLibrary(session, cfg.Keyspace))
		r.Post("/scan", func(w http.ResponseWriter, r *http.Request) {
			added, err := scanWithLibraries(r.Context(), mediaSvc, session, cfg.Keyspace, cfg.MediaRoot)
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

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			_, _ = scanWithLibraries(context.Background(), mediaSvc, session, cfg.Keyspace, cfg.MediaRoot)
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

func scanWithLibraries(ctx context.Context, svc *media.Service, session *gocql.Session, keyspace, fallback string) (int, error) {
	roots, err := loadLibraryRoots(ctx, session, keyspace, fallback)
	if err != nil {
		return 0, err
	}
	if len(roots) == 0 {
		return 0, nil
	}
	return svc.ScanRoots(ctx, roots)
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
      font-family: "Palatino Linotype", "Book Antiqua", Palatino, serif;
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
    .card-title { font-weight: bold; margin-bottom: 10px; }
    .meta { color: var(--grey); font-size: 12px; }
    .player-overlay {
      position: fixed;
      inset: 0;
      background: rgba(0, 0, 0, 0.7);
      display: none;
      align-items: center;
      justify-content: center;
      z-index: 999;
      padding: 20px;
    }
    .player-shell {
      width: min(960px, 100%);
      background: #111;
      border-radius: 16px;
      overflow: hidden;
      border: 1px solid #333;
    }
    .player-bar {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding: 8px 12px;
      background: #1c1c1c;
      color: #fff;
      font-size: 13px;
    }
    .player-close {
      background: #2b2b2b;
      color: #fff;
      border: 1px solid #3a3a3a;
    }
    video { width: 100%; height: auto; display: block; }
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
        <div class="subtitle">Bibliotheque locale + streaming (MVP)</div>
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
      <div class="panel">
        <div id="status" class="status"></div>
        <div id="token" class="token"></div>
      </div>
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
  </div>
  <div id="playerOverlay" class="player-overlay">
    <div class="player-shell">
      <div class="player-bar">
        <div id="playerTitle">Lecture</div>
        <button id="playerClose" class="player-close">Fermer</button>
      </div>
      <video id="playerVideo" controls playsinline></video>
    </div>
  </div>
<script>
let access = localStorage.getItem('access_token') || '';
let refreshToken = localStorage.getItem('refresh_token') || '';
const statusEl = document.getElementById('status');
const tokenEl = document.getElementById('token');
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
  tokenEl.textContent = access ? ('Token: ' + access) : 'Token: (empty)';
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
  statusEl.textContent = msg;
  statusEl.style.color = isError ? '#b00' : '#060';
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
    tokenEl.textContent = 'Token: ' + access;
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
    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = m.title + ' (' + (m.year || '') + ')';
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.textContent = 'Type: ' + m.type;
    const btn = document.createElement('button');
    btn.textContent = 'Play';
    btn.addEventListener('click', () => play(m.id, m.title));
    el.appendChild(title);
    el.appendChild(meta);
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
  const url='/stream?path='+encodeURIComponent(assets[0].path)+'&token='+encodeURIComponent(access);
  openPlayer(url, titleText || 'Lecture');
}
async function scanNow(){
  if (!access) { setStatus('Not logged in', true); return; }
  const res = await fetch('/admin/scan',{method:'POST',headers:{Authorization:'Bearer '+access}});
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
const playerVideo = document.getElementById('playerVideo');
const playerTitle = document.getElementById('playerTitle');
function openPlayer(url, titleText){
  playerTitle.textContent = titleText;
  playerVideo.src = url;
  overlay.style.display = 'flex';
  playerVideo.play().catch(()=>{});
}
function closePlayer(){
  playerVideo.pause();
  playerVideo.removeAttribute('src');
  playerVideo.load();
  overlay.style.display = 'none';
}
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
