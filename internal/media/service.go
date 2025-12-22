package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

type ProgressItem struct {
	Item       Item      `json:"item"`
	PositionMs int64     `json:"position_ms"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Service struct {
	session   *gocql.Session
	keyspace  string
	mediaRoot string
	tmdbKey   string
}

type LibraryRoot struct {
	Path string
	Kind string
}

func NewService(session *gocql.Session, keyspace, mediaRoot, tmdbKey string) *Service {
	return &Service{session: session, keyspace: keyspace, mediaRoot: mediaRoot, tmdbKey: tmdbKey}
}

func (s *Service) List(ctx context.Context, query string, limit int) ([]Item, error) {
	items := make([]Item, 0)
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

func (s *Service) ListContinue(ctx context.Context, userID string, limit int) ([]ProgressItem, error) {
	iter := s.session.Query(fmt.Sprintf(`SELECT media_id, position_ms, updated_at FROM %s.play_state WHERE user_id=?`, s.keyspace), userID).
		WithContext(ctx).Iter()
	var mediaID gocql.UUID
	var pos int64
	var updated time.Time
	items := []ProgressItem{}
	for iter.Scan(&mediaID, &pos, &updated) {
		it, err := s.Get(ctx, mediaID.String())
		if err != nil {
			continue
		}
		items = append(items, ProgressItem{
			Item:       it,
			PositionMs: pos,
			UpdatedAt:  updated,
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *Service) Reset(ctx context.Context) error {
	tables := []string{"media_assets", "media_paths", "media_items", "play_state"}
	for _, table := range tables {
		if err := s.session.Query(fmt.Sprintf("TRUNCATE %s.%s", s.keyspace, table)).WithContext(ctx).Exec(); err != nil {
			return err
		}
	}
	return nil
}

// Scan walks the media root and inserts items/assets.
func (s *Service) Scan(ctx context.Context) (int, error) {
	if s.mediaRoot == "" {
		return 0, fmt.Errorf("media root not configured")
	}
	return s.ScanRoots(ctx, []string{s.mediaRoot}, true)
}

// ScanRoots walks the provided roots and inserts items/assets.
func (s *Service) ScanRoots(ctx context.Context, roots []string, refreshExisting bool) (int, error) {
	libs := make([]LibraryRoot, 0, len(roots))
	for _, root := range roots {
		libs = append(libs, LibraryRoot{Path: root, Kind: "movie"})
	}
	return s.ScanLibraries(ctx, libs, refreshExisting)
}

// ScanLibraries walks the provided library roots and inserts items/assets.
func (s *Service) ScanLibraries(ctx context.Context, libs []LibraryRoot, refreshExisting bool) (int, error) {
	added := 0
	if len(libs) == 0 {
		return 0, fmt.Errorf("no media roots configured")
	}
	for _, lib := range libs {
		root := strings.TrimSpace(lib.Path)
		kind := strings.ToLower(strings.TrimSpace(lib.Kind))
		if kind != "movie" && kind != "series" {
			kind = "movie"
		}
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !isVideo(path) {
				return nil
			}
			absPath := filepath.Clean(path)
			relPath := ""
			if rel, relErr := filepath.Rel(root, absPath); relErr == nil {
				relPath = filepath.Clean(rel)
			}
			title, year := parseTitle(info.Name())
			if existingID, ok := s.lookupMediaID(ctx, relPath, absPath); ok {
				inserted, err := s.ensureMediaForPath(ctx, existingID, absPath, info, title, year, kind, refreshExisting)
				if err != nil {
					return err
				}
				if inserted {
					added++
				}
				if relPath != "" {
					if err := s.ensurePathAlias(ctx, relPath, existingID); err != nil {
						return err
					}
				}
				if err := s.ensurePathAlias(ctx, absPath, existingID); err != nil {
					return err
				}
				return nil
			}

			mediaID := gocql.TimeUUID()
			var existingPath string
			var existingID gocql.UUID
			applied, err := s.session.Query(
				fmt.Sprintf(`INSERT INTO %s.media_paths (path, media_id) VALUES (?, ?) IF NOT EXISTS`, s.keyspace),
				absPath, mediaID,
			).WithContext(ctx).ScanCAS(&existingPath, &existingID)
			if err != nil {
				return err
			}
			if !applied {
				inserted, err := s.ensureMediaForPath(ctx, existingID, absPath, info, title, year, kind, refreshExisting)
				if err != nil {
					return err
				}
				if inserted {
					added++
				}
				if relPath != "" {
					if err := s.ensurePathAlias(ctx, relPath, existingID); err != nil {
						return err
					}
				}
				return nil
			}
			if err := s.session.Query(fmt.Sprintf(`INSERT INTO %s.media_items (id,type,title,year,created_at) VALUES (?,?,?,?,?)`, s.keyspace),
				mediaID, kind, title, year, time.Now()).WithContext(ctx).Exec(); err != nil {
				return err
			}
			if err := s.session.Query(fmt.Sprintf(`INSERT INTO %s.media_assets (id,media_id,path,size,format) VALUES (?,?,?,?,?)`, s.keyspace),
				gocql.TimeUUID(), mediaID, absPath, info.Size(), filepath.Ext(absPath)).WithContext(ctx).Exec(); err != nil {
				return err
			}
			if err := s.enrichMetadata(ctx, mediaID, absPath, title, year); err != nil {
				return err
			}
			if relPath != "" {
				if err := s.ensurePathAlias(ctx, relPath, mediaID); err != nil {
					return err
				}
			}
			added++
			return nil
		})
		if err != nil {
			return added, err
		}
	}
	return added, nil
}

func (s *Service) ensureMediaForPath(ctx context.Context, mediaID gocql.UUID, path string, info os.FileInfo, title string, year int, kind string, refreshExisting bool) (bool, error) {
	inserted := false
	var existing string
	var currentTitle string
	var currentYear int
	var currentPoster string
	var currentMeta map[string]string
	var currentType string
	err := s.session.Query(fmt.Sprintf(`SELECT id,type,title,year,poster_url,metadata FROM %s.media_items WHERE id=?`, s.keyspace), mediaID).
		WithContext(ctx).Scan(&existing, &currentType, &currentTitle, &currentYear, &currentPoster, &currentMeta)
	if err != nil {
		if !errors.Is(err, gocql.ErrNotFound) {
			return false, err
		}
		if err := s.session.Query(fmt.Sprintf(`INSERT INTO %s.media_items (id,type,title,year,created_at) VALUES (?,?,?,?,?)`, s.keyspace),
			mediaID, kind, title, year, time.Now()).WithContext(ctx).Exec(); err != nil {
			return false, err
		}
		inserted = true
	} else {
		if kind != "" && currentType != kind {
			if err := s.session.Query(fmt.Sprintf(`UPDATE %s.media_items SET type=? WHERE id=?`, s.keyspace),
				kind, mediaID).WithContext(ctx).Exec(); err != nil {
				return false, err
			}
		}
		if refreshExisting && title != "" && (currentTitle != title || (year > 0 && currentYear != year)) {
			if err := s.session.Query(fmt.Sprintf(`UPDATE %s.media_items SET title=?, year=? WHERE id=?`, s.keyspace),
				title, year, mediaID).WithContext(ctx).Exec(); err != nil {
				return false, err
			}
		}
	}

	if inserted || refreshExisting || currentPoster == "" || len(currentMeta) == 0 {
		if err := s.enrichMetadata(ctx, mediaID, path, title, year); err != nil {
			return false, err
		}
	}

	found := false
	iter := s.session.Query(fmt.Sprintf(`SELECT id,path FROM %s.media_assets WHERE media_id=?`, s.keyspace), mediaID).
		WithContext(ctx).Iter()
	var assetID gocql.UUID
	var assetPath string
	for iter.Scan(&assetID, &assetPath) {
		if assetPath == path {
			found = true
			break
		}
	}
	if err := iter.Close(); err != nil {
		return false, err
	}
	if found {
		return inserted, nil
	}
	if err := s.session.Query(fmt.Sprintf(`INSERT INTO %s.media_assets (id,media_id,path,size,format) VALUES (?,?,?,?,?)`, s.keyspace),
		gocql.TimeUUID(), mediaID, path, info.Size(), filepath.Ext(path)).WithContext(ctx).Exec(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) lookupMediaID(ctx context.Context, relPath, absPath string) (gocql.UUID, bool) {
	if relPath != "" {
		var id gocql.UUID
		if err := s.session.Query(fmt.Sprintf(`SELECT media_id FROM %s.media_paths WHERE path=?`, s.keyspace), relPath).
			WithContext(ctx).Scan(&id); err == nil {
			return id, true
		}
	}
	if absPath != "" {
		var id gocql.UUID
		if err := s.session.Query(fmt.Sprintf(`SELECT media_id FROM %s.media_paths WHERE path=?`, s.keyspace), absPath).
			WithContext(ctx).Scan(&id); err == nil {
			return id, true
		}
	}
	return gocql.UUID{}, false
}

func (s *Service) ensurePathAlias(ctx context.Context, path string, mediaID gocql.UUID) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	var existingPath string
	var existingID gocql.UUID
	applied, err := s.session.Query(
		fmt.Sprintf(`INSERT INTO %s.media_paths (path, media_id) VALUES (?, ?) IF NOT EXISTS`, s.keyspace),
		path, mediaID,
	).WithContext(ctx).ScanCAS(&existingPath, &existingID)
	if err != nil {
		return err
	}
	if !applied && existingID != mediaID {
		return fmt.Errorf("path alias already mapped to another media")
	}
	return nil
}

func (s *Service) enrichMetadata(ctx context.Context, mediaID gocql.UUID, filePath, title string, year int) error {
	nfoTitle, nfoYear, imdbID := readNFO(filePath)
	updatedTitle := title
	updatedYear := year
	if nfoTitle != "" {
		updatedTitle = nfoTitle
	}
	if nfoYear > 0 {
		updatedYear = nfoYear
	}
	if updatedTitle != title || updatedYear != year {
		if err := s.session.Query(fmt.Sprintf(`UPDATE %s.media_items SET title=?, year=? WHERE id=?`, s.keyspace),
			updatedTitle, updatedYear, mediaID).WithContext(ctx).Exec(); err != nil {
			return err
		}
	}
	if s.tmdbKey == "" {
		if imdbID != "" {
			if err := s.session.Query(fmt.Sprintf(`UPDATE %s.media_items SET metadata=? WHERE id=?`, s.keyspace),
				map[string]string{"imdb_id": imdbID}, mediaID).WithContext(ctx).Exec(); err != nil {
				return err
			}
		}
		return nil
	}
	meta, poster, backdrop, err := s.fetchTmdbMetadata(updatedTitle, updatedYear, imdbID)
	if err != nil {
		return nil
	}
	if len(meta) == 0 && poster == "" && backdrop == "" {
		return nil
	}
	tmdbTitle := strings.TrimSpace(meta["title"])
	tmdbYear := 0
	if metaYear := strings.TrimSpace(meta["year"]); metaYear != "" {
		fmt.Sscanf(metaYear, "%d", &tmdbYear)
	}
	if tmdbTitle != "" && (tmdbTitle != updatedTitle || (tmdbYear > 0 && tmdbYear != updatedYear)) {
		titleToSet := updatedTitle
		yearToSet := updatedYear
		if tmdbTitle != "" {
			titleToSet = tmdbTitle
		}
		if tmdbYear > 0 {
			yearToSet = tmdbYear
		}
		if err := s.session.Query(fmt.Sprintf(`UPDATE %s.media_items SET title=?, year=? WHERE id=?`, s.keyspace),
			titleToSet, yearToSet, mediaID).WithContext(ctx).Exec(); err != nil {
			return err
		}
	}
	if poster != "" || backdrop != "" {
		return s.session.Query(fmt.Sprintf(`UPDATE %s.media_items SET metadata=?, poster_url=?, backdrop_url=? WHERE id=?`, s.keyspace),
			meta, poster, backdrop, mediaID).WithContext(ctx).Exec()
	}
	return s.session.Query(fmt.Sprintf(`UPDATE %s.media_items SET metadata=? WHERE id=?`, s.keyspace),
		meta, mediaID).WithContext(ctx).Exec()
}

func readNFO(filePath string) (string, int, string) {
	dir := filepath.Dir(filePath)
	base := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	candidates := []string{
		filepath.Join(dir, base+".nfo"),
		filepath.Join(dir, "movie.nfo"),
	}
	for _, c := range candidates {
		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		text := string(data)
		title := extractTag(text, "title")
		yearStr := extractTag(text, "year")
		imdbID := extractTag(text, "imdbid")
		if imdbID == "" {
			imdbID = extractTag(text, "imdb_id")
		}
		year := 0
		if yearStr != "" {
			fmt.Sscanf(yearStr, "%d", &year)
		}
		return strings.TrimSpace(title), year, strings.TrimSpace(imdbID)
	}
	return "", 0, ""
}

func extractTag(text, tag string) string {
	low := strings.ToLower(text)
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(low, open)
	if start == -1 {
		return ""
	}
	start += len(open)
	end := strings.Index(low[start:], close)
	if end == -1 {
		return ""
	}
	return text[start : start+end]
}

func (s *Service) fetchTmdbMetadata(title string, year int, imdbID string) (map[string]string, string, string, error) {
	if s.tmdbKey == "" {
		return nil, "", "", fmt.Errorf("tmdb key missing")
	}
	var tmdbID int
	if imdbID != "" {
		id, err := s.tmdbIDFromImdb(imdbID)
		if err == nil {
			tmdbID = id
		}
	}
	if tmdbID == 0 {
		id, err := s.tmdbIDFromSearch(title, year)
		if err == nil {
			tmdbID = id
		}
	}
	if tmdbID == 0 {
		return nil, "", "", fmt.Errorf("tmdb not found")
	}
	meta, poster, backdrop, err := s.tmdbMovieDetails(tmdbID)
	if err != nil {
		return nil, "", "", err
	}
	credits, err := s.tmdbMovieCredits(tmdbID)
	if err == nil {
		for k, v := range credits {
			meta[k] = v
		}
	}
	if imdbID != "" && meta["imdb_id"] == "" {
		meta["imdb_id"] = imdbID
	}
	return meta, poster, backdrop, nil
}

func (s *Service) tmdbIDFromImdb(imdbID string) (int, error) {
	endpoint := "https://api.themoviedb.org/3/find/" + url.PathEscape(imdbID)
	params := url.Values{}
	params.Set("api_key", s.tmdbKey)
	params.Set("external_source", "imdb_id")
	params.Set("language", "fr-FR")
	reqURL := endpoint + "?" + params.Encode()
	body, err := getJSON(reqURL)
	if err != nil {
		return 0, err
	}
	var out struct {
		MovieResults []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, err
	}
	if len(out.MovieResults) == 0 {
		return 0, fmt.Errorf("tmdb not found")
	}
	return out.MovieResults[0].ID, nil
}

func (s *Service) tmdbIDFromSearch(title string, year int) (int, error) {
	endpoint := "https://api.themoviedb.org/3/search/movie"
	params := url.Values{}
	params.Set("api_key", s.tmdbKey)
	params.Set("query", title)
	params.Set("language", "fr-FR")
	if year > 0 {
		params.Set("year", fmt.Sprintf("%d", year))
	}
	reqURL := endpoint + "?" + params.Encode()
	body, err := getJSON(reqURL)
	if err != nil {
		return 0, err
	}
	var out struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, err
	}
	if len(out.Results) == 0 {
		return 0, fmt.Errorf("tmdb not found")
	}
	return out.Results[0].ID, nil
}

func (s *Service) tmdbMovieDetails(id int) (map[string]string, string, string, error) {
	endpoint := fmt.Sprintf("https://api.themoviedb.org/3/movie/%d", id)
	params := url.Values{}
	params.Set("api_key", s.tmdbKey)
	params.Set("language", "fr-FR")
	reqURL := endpoint + "?" + params.Encode()
	body, err := getJSON(reqURL)
	if err != nil {
		return nil, "", "", err
	}
	var out struct {
		ID               int     `json:"id"`
		Title            string  `json:"title"`
		OriginalTitle    string  `json:"original_title"`
		ReleaseDate      string  `json:"release_date"`
		Overview         string  `json:"overview"`
		Tagline          string  `json:"tagline"`
		Runtime          int     `json:"runtime"`
		VoteAverage      float64 `json:"vote_average"`
		PosterPath       string  `json:"poster_path"`
		BackdropPath     string  `json:"backdrop_path"`
		ImdbID           string  `json:"imdb_id"`
		Genres           []struct {
			Name string `json:"name"`
		} `json:"genres"`
		ProductionCompanies []struct {
			Name string `json:"name"`
		} `json:"production_companies"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, "", "", err
	}
	genres := make([]string, 0, len(out.Genres))
	for _, g := range out.Genres {
		if strings.TrimSpace(g.Name) != "" {
			genres = append(genres, g.Name)
		}
	}
	studios := make([]string, 0, len(out.ProductionCompanies))
	for _, s := range out.ProductionCompanies {
		if strings.TrimSpace(s.Name) != "" {
			studios = append(studios, s.Name)
		}
	}
	genresJSON, _ := json.Marshal(genres)
	studiosJSON, _ := json.Marshal(studios)
	meta := map[string]string{
		"title":          out.Title,
		"original_title": out.OriginalTitle,
		"year":           yearFromDate(out.ReleaseDate),
		"plot":           out.Overview,
		"tagline":        out.Tagline,
		"runtime":        fmt.Sprintf("%d", out.Runtime),
		"rating":         fmt.Sprintf("%.1f", out.VoteAverage),
		"tmdb_id":        fmt.Sprintf("%d", out.ID),
		"imdb_id":        out.ImdbID,
		"genres_json":    string(genresJSON),
		"studios_json":   string(studiosJSON),
		"type":           "movie",
	}
	return meta, tmdbPosterURL(out.PosterPath), tmdbBackdropURL(out.BackdropPath), nil
}

func (s *Service) tmdbMovieCredits(id int) (map[string]string, error) {
	endpoint := fmt.Sprintf("https://api.themoviedb.org/3/movie/%d/credits", id)
	params := url.Values{}
	params.Set("api_key", s.tmdbKey)
	params.Set("language", "fr-FR")
	reqURL := endpoint + "?" + params.Encode()
	body, err := getJSON(reqURL)
	if err != nil {
		return nil, err
	}
	var out struct {
		Cast []struct {
			Name      string `json:"name"`
			Character string `json:"character"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Job  string `json:"job"`
		} `json:"crew"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	directorSet := map[string]bool{}
	writerSet := map[string]bool{}
	for _, c := range out.Crew {
		job := strings.ToLower(strings.TrimSpace(c.Job))
		if job == "director" {
			directorSet[c.Name] = true
		}
		if job == "writer" || job == "screenplay" {
			writerSet[c.Name] = true
		}
	}
	directors := make([]string, 0, len(directorSet))
	for name := range directorSet {
		directors = append(directors, name)
	}
	writers := make([]string, 0, len(writerSet))
	for name := range writerSet {
		writers = append(writers, name)
	}
	castList := make([]map[string]string, 0, len(out.Cast))
	for i, c := range out.Cast {
		if i >= 20 {
			break
		}
		castList = append(castList, map[string]string{
			"name": c.Name,
			"character": c.Character,
		})
	}
	castJSON, _ := json.Marshal(castList)
	meta := map[string]string{
		"director":  strings.Join(directors, ", "),
		"writer":    strings.Join(writers, ", "),
		"cast_json": string(castJSON),
	}
	return meta, nil
}

func (s *Service) DebugTmdb(title string, year int, imdbID string) (map[string]interface{}, error) {
	if s.tmdbKey == "" {
		return nil, fmt.Errorf("tmdb key missing")
	}
	if imdbID != "" {
		endpoint := "https://api.themoviedb.org/3/find/" + url.PathEscape(imdbID)
		params := url.Values{}
		params.Set("api_key", s.tmdbKey)
		params.Set("external_source", "imdb_id")
		params.Set("language", "fr-FR")
		reqURL := endpoint + "?" + params.Encode()
		body, err := getJSON(reqURL)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"url":  redactApiKey(reqURL),
			"body": string(body),
		}, nil
	}
	endpoint := "https://api.themoviedb.org/3/search/movie"
	params := url.Values{}
	params.Set("api_key", s.tmdbKey)
	params.Set("query", title)
	params.Set("language", "fr-FR")
	if year > 0 {
		params.Set("year", fmt.Sprintf("%d", year))
	}
	reqURL := endpoint + "?" + params.Encode()
	body, err := getJSON(reqURL)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"url":  redactApiKey(reqURL),
		"body": string(body),
	}, nil
}

func getJSON(url string) ([]byte, error) {
	log.Printf("tmdb request: %s", redactApiKey(url))
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("tmdb response status=%d", resp.StatusCode)
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("tmdb response: %s", snippet(body, 300))
	return body, nil
}

func tmdbPosterURL(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w500" + path
}

func tmdbBackdropURL(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w1280" + path
}

func yearFromDate(date string) string {
	if len(date) < 4 {
		return ""
	}
	return date[:4]
}

func redactApiKey(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Has("api_key") {
		q.Set("api_key", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func snippet(data []byte, max int) string {
	if len(data) <= max {
		return string(data)
	}
	return string(data[:max]) + "..."
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
	base = stripBracketed(base)
	parts := strings.Fields(base)
	year := 0
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if len(last) == 4 && strings.HasPrefix(last, "19") || strings.HasPrefix(last, "20") {
			fmt.Sscanf(last, "%d", &year)
			parts = parts[:len(parts)-1]
		}
	}
	parts = filterNoise(parts)
	title := strings.Title(strings.Join(parts, " "))
	if title == "" {
		title = base
	}
	return title, year
}

func stripBracketed(s string) string {
	out := s
	for _, pair := range []struct{ open, close string }{
		{"[", "]"},
		{"(", ")"},
		{"{", "}"},
	} {
		for {
			start := strings.Index(out, pair.open)
			if start == -1 {
				break
			}
			end := strings.Index(out[start+1:], pair.close)
			if end == -1 {
				break
			}
			out = strings.TrimSpace(out[:start] + " " + out[start+1+end+1:])
		}
	}
	return out
}

func filterNoise(parts []string) []string {
	blacklist := map[string]bool{
		"1080p": true, "2160p": true, "720p": true, "480p": true,
		"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true,
		"bluray": true, "brrip": true, "webrip": true, "webdl": true, "hdrip": true,
		"dvdrip": true, "remux": true, "hdr": true, "dv": true, "dolby": true,
		"dts": true, "truehd": true, "atmos": true, "yify": true, "rarbg": true,
		"proper": true, "repack": true, "extended": true, "unrated": true,
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		key := strings.ToLower(strings.Trim(p, "-_"))
		if key == "" || blacklist[key] {
			continue
		}
		if strings.HasPrefix(key, "s") && strings.Contains(key, "e") {
			continue
		}
		out = append(out, p)
	}
	return out
}
