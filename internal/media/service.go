package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocql/gocql"
)

var ErrNotFound = errors.New("not found")

type Item struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Title       string            `json:"title"`
	Year        int               `json:"year,omitempty"`
	Season      int               `json:"season,omitempty"`
	Episode     int               `json:"episode,omitempty"`
	ShowID      string            `json:"show_id,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	PosterURL   string            `json:"poster_url,omitempty"`
	BackdropURL string            `json:"backdrop_url,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

type Asset struct {
	ID     string `json:"id"`
	Media  string `json:"media_id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Format string `json:"format"`
}

type Service struct {
	session   *gocql.Session
	keyspace  string
	mediaRoot string
}

func NewService(session *gocql.Session, keyspace, mediaRoot string) *Service {
	return &Service{session: session, keyspace: keyspace, mediaRoot: mediaRoot}
}

func (s *Service) List(ctx context.Context, query string, limit int) ([]Item, error) {
	var items []Item
	q := s.session.Query(fmt.Sprintf(`SELECT id,type,title,year,season,episode,show_id,metadata,poster_url,backdrop_url,created_at FROM %s.media_items LIMIT ?`, s.keyspace), limit)
	iter := q.WithContext(ctx).Iter()
	var it Item
	for iter.Scan(&it.ID, &it.Type, &it.Title, &it.Year, &it.Season, &it.Episode, &it.ShowID, &it.Metadata, &it.PosterURL, &it.BackdropURL, &it.CreatedAt) {
		if query == "" || strings.Contains(strings.ToLower(it.Title), strings.ToLower(query)) {
			items = append(items, it)
		}
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Service) Get(ctx context.Context, id string) (Item, error) {
	var it Item
	err := s.session.Query(fmt.Sprintf(`SELECT id,type,title,year,season,episode,show_id,metadata,poster_url,backdrop_url,created_at FROM %s.media_items WHERE id=?`, s.keyspace), id).
		WithContext(ctx).
		Scan(&it.ID, &it.Type, &it.Title, &it.Year, &it.Season, &it.Episode, &it.ShowID, &it.Metadata, &it.PosterURL, &it.BackdropURL, &it.CreatedAt)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return Item{}, ErrNotFound
		}
		return Item{}, err
	}
	return it, nil
}

func (s *Service) Assets(ctx context.Context, mediaID string) ([]Asset, error) {
	var assets []Asset
	iter := s.session.Query(fmt.Sprintf(`SELECT id,media_id,path,size,format FROM %s.media_assets WHERE media_id=?`, s.keyspace), mediaID).
		WithContext(ctx).Iter()
	var a Asset
	for iter.Scan(&a.ID, &a.Media, &a.Path, &a.Size, &a.Format) {
		assets = append(assets, a)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return assets, nil
}

func (s *Service) UpdateProgress(ctx context.Context, userID, mediaID string, pos int64) error {
	return s.session.Query(fmt.Sprintf(`UPDATE %s.play_state SET position_ms=?, updated_at=? WHERE user_id=? AND media_id=?`, s.keyspace),
		pos, time.Now(), userID, mediaID).WithContext(ctx).Exec()
}

// Scan walks the media root and inserts items/assets.
func (s *Service) Scan(ctx context.Context) (int, error) {
	added := 0
	err := filepath.Walk(s.mediaRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !isVideo(path) {
			return nil
		}
		rel, _ := filepath.Rel(s.mediaRoot, path)
		title, year := parseTitle(info.Name())
		mediaID := gocql.TimeUUID()
		assetID := gocql.TimeUUID()
		var existingPath string
		var existingID gocql.UUID
		applied, err := s.session.Query(
			fmt.Sprintf(`INSERT INTO %s.media_paths (path, media_id) VALUES (?, ?) IF NOT EXISTS`, s.keyspace),
			rel, mediaID,
		).WithContext(ctx).ScanCAS(&existingPath, &existingID)
		if err != nil {
			return err
		}
		if !applied {
			// already indexed
			return nil
		}
		if err := s.session.Query(fmt.Sprintf(`INSERT INTO %s.media_items (id,type,title,year,created_at) VALUES (?,?,?,?,?)`, s.keyspace),
			mediaID, "movie", title, year, time.Now()).WithContext(ctx).Exec(); err != nil {
			return err
		}
		if err := s.session.Query(fmt.Sprintf(`INSERT INTO %s.media_assets (id,media_id,path,size,format) VALUES (?,?,?,?,?)`, s.keyspace),
			assetID, mediaID, rel, info.Size(), filepath.Ext(path)).WithContext(ctx).Exec(); err != nil {
			return err
		}
		added++
		return nil
	})
	if err != nil {
		return added, err
	}
	return added, nil
}

func isVideo(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".mkv", ".avi", ".mov":
		return true
	default:
		return false
	}
}

func parseTitle(name string) (string, int) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = strings.ReplaceAll(base, ".", " ")
	parts := strings.Fields(base)
	year := 0
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if len(last) == 4 && strings.HasPrefix(last, "19") || strings.HasPrefix(last, "20") {
			fmt.Sscanf(last, "%d", &year)
			parts = parts[:len(parts)-1]
		}
	}
	title := strings.Title(strings.Join(parts, " "))
	if title == "" {
		title = base
	}
	return title, year
}
