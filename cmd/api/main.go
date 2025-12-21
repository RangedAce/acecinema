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
		MediaRoot:   envDefault("MEDIA_ROOT", "/mnt/media"),
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

    r.With(authSvc.RequireRole("admin")).Post("/scan", func(w http.ResponseWriter, r *http.Request) {
        added, err := mediaSvc.Scan(context.Background())
        if err != nil {
            errorJSON(w, http.StatusInternalServerError, err.Error())
            return
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
            "status": "scan completed",
            "added":  added,
        })
    })

	r.With(authSvc.RequireAuth).Get("/stream", handleStream(mediaSvc, cfg.MediaRoot))

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

func handleStream(svc *media.Service, mediaRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			errorJSON(w, http.StatusBadRequest, "path required")
			return
		}
		full := filepath.Clean(filepath.Join(mediaRoot, path))
		if !strings.HasPrefix(full, filepath.Clean(mediaRoot)) {
			errorJSON(w, http.StatusForbidden, "invalid path")
			return
		}
		http.ServeFile(w, r, full)
	}
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
    body { font-family: Arial, sans-serif; margin: 20px; }
    input, button { margin: 4px; }
    .card { border: 1px solid #ccc; padding: 8px; margin: 6px 0; }
  </style>
</head>
<body>
  <h1>AceCinema (MVP)</h1>
  <div id="auth">
    <input id="email" placeholder="email" value="admin@example.com"/>
    <input id="password" placeholder="password" type="password" value="changeme-admin"/>
    <button id="loginBtn">Login</button>
    <button id="refreshBtn">Refresh token</button>
    <button id="logoutBtn">Logout</button>
  </div>
  <div>
    <button id="loadBtn">Charger les medias</button>
    <button id="scanBtn">Scanner maintenant</button>
  </div>
  <div id="token" style="margin-top:8px; font-family: monospace;"></div>
  <div id="media"></div>
<script>
let access = localStorage.getItem('access_token') || '';
let refreshToken = localStorage.getItem('refresh_token') || '';
document.getElementById('token').textContent = access ? ('Token: ' + access) : 'Token: (empty)';
async function login() {
  const res = await fetch('/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({email:email.value,password:password.value})});
  const data = await res.json();
  access = data.access_token||'';
  refreshToken = data.refresh_token||'';
  localStorage.setItem('access_token', access);
  localStorage.setItem('refresh_token', refreshToken);
  document.getElementById('token').textContent = access ? ('Token: ' + access) : 'Token: (empty)';
  alert('login ' + (res.ok?'ok':'ko'));
}
async function refresh() {
  if (!refreshToken) { alert('no refresh token'); return; }
  const res = await fetch('/auth/refresh',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({refresh_token:refreshToken})});
  const data = await res.json(); access=data.access_token||''; alert('refresh ' + (res.ok?'ok':'ko'));
  if (access) {
    localStorage.setItem('access_token', access);
    document.getElementById('token').textContent = 'Token: ' + access;
  }
}
async function loadMedia() {
  if (!access) { alert('not logged in'); return; }
  const res = await fetch('/media',{headers:{Authorization:'Bearer '+access}});
  if (!res.ok) { alert('load failed: ' + res.status); return; }
  const list = await res.json();
  const div = document.getElementById('media'); div.innerHTML='';
  (list||[]).forEach(m=>{
    const el=document.createElement('div');
    el.className='card';
    const title=document.createElement('div');
    title.innerHTML='<strong>'+m.title+'</strong> ('+(m.year||'')+') - '+m.type;
    const btn=document.createElement('button');
    btn.textContent='Play';
    btn.addEventListener('click',()=>play(m.id));
    el.appendChild(title);
    el.appendChild(btn);
    div.appendChild(el);
  });
}
async function play(id){
  const assets = await fetch('/media/'+id+'/assets',{headers:{Authorization:'Bearer '+access}}).then(r=>r.json());
  if(assets.length===0){alert('no assets'); return;}
  const url='/stream?path='+encodeURIComponent(assets[0].path);
  window.open(url,'_blank');
}
async function scanNow(){
  if (!access) { alert('not logged in'); return; }
  const res = await fetch('/scan',{method:'POST',headers:{Authorization:'Bearer '+access}});
  const text = await res.text();
  alert('scan ' + (res.ok?'started':'failed') + ' ' + text);
}
function logout(){
  access = '';
  refreshToken = '';
  localStorage.removeItem('access_token');
  localStorage.removeItem('refresh_token');
  document.getElementById('token').textContent = 'Token: (empty)';
}
document.getElementById('loginBtn').addEventListener('click', login);
document.getElementById('refreshBtn').addEventListener('click', refresh);
document.getElementById('logoutBtn').addEventListener('click', logout);
document.getElementById('loadBtn').addEventListener('click', loadMedia);
document.getElementById('scanBtn').addEventListener('click', scanNow);
</script>
</body>
</html>`)
}
