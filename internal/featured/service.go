package featured

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"acecinema/internal/db"
	"acecinema/internal/media"
)

type Service struct {
	session  *gocql.Session
	keyspace string
	cfg      Config
	cache    *cacheStore
}

func NewService(session *gocql.Session, keyspace string, cfg Config) *Service {
	return &Service{
		session:  session,
		keyspace: keyspace,
		cfg:      cfg.normalize(),
		cache:    newCache(),
	}
}

func (s *Service) Config() Config {
	return s.cfg
}

func (s *Service) ItemsForUser(ctx context.Context, userID string, now time.Time) ([]Item, PublicConfig, error) {
	cfg := s.cfg.normalize()
	cacheKey := s.cacheKey(userID, cfg, now)
	if cached, ok := s.cache.Get(cacheKey, now); ok {
		return cached, cfg.Public(), nil
	}

	items, err := s.selectItems(ctx, userID, cfg, now)
	if err != nil {
		return nil, cfg.Public(), err
	}

	ttl := cfg.CacheTTLRecent
	if isRandomMode(cfg.Mode) {
		ttl = cfg.CacheTTLRandom
	}
	s.cache.Set(cacheKey, items, ttl, now)
	return items, cfg.Public(), nil
}

func (s *Service) cacheKey(userID string, cfg Config, now time.Time) string {
	base := userID + ":" + cfg.selectionHash()
	if isRandomMode(cfg.Mode) && strings.EqualFold(cfg.RandomSeedStrategy, RandomDailyPerUser) {
		day := now.UTC().Format("2006-01-02")
		return base + ":" + day
	}
	return base
}

func (c Config) selectionHash() string {
	payload := struct {
		Mode                  Mode
		Limit                 int
		LibrariesAllowed      []string
		ExcludePlayed         bool
		MinCommunityRating    float64
		MinCriticRating       int
		ParentalRatingAllowed []string
		MaxParentalRating     string
		RandomSeedStrategy    string
		PeriodDays            int
		ImageResizeEnabled    bool
	}{
		Mode:                  c.Mode,
		Limit:                 c.Limit,
		LibrariesAllowed:      c.LibrariesAllowed,
		ExcludePlayed:         c.ExcludePlayed,
		MinCommunityRating:    c.MinCommunityRating,
		MinCriticRating:       c.MinCriticRating,
		ParentalRatingAllowed: c.ParentalRatingAllowed,
		MaxParentalRating:     c.MaxParentalRating,
		RandomSeedStrategy:    c.RandomSeedStrategy,
		PeriodDays:            c.PeriodDays,
		ImageResizeEnabled:    c.ImageResizeEnabled,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Service) selectItems(ctx context.Context, userID string, cfg Config, now time.Time) ([]Item, error) {
	allItems, err := s.listItems(ctx)
	if err != nil {
		return nil, err
	}
	if len(allItems) == 0 {
		return []Item{}, nil
	}

	played, _ := s.listPlayState(ctx, userID)
	allowedLibraries, _ := s.loadLibraries(ctx)
	allowedRoots := buildAllowedRoots(cfg, allowedLibraries)

	candidates := make([]media.Item, 0, len(allItems))
	// TODO: apply per-user permission filtering when available.
	for _, item := range allItems {
		if !passesLibraryFilter(ctx, s, item, allowedRoots) {
			continue
		}
		if !passesParentalFilter(item, cfg) {
			continue
		}
		if cfg.ExcludePlayed && played[item.ID] > 0 {
			continue
		}
		if !passesRatingFilter(item, cfg) {
			continue
		}
		candidates = append(candidates, item)
	}

	if len(candidates) == 0 {
		// Relax filters to avoid empty hero.
		candidates = s.relaxCandidates(ctx, allItems, cfg, allowedRoots)
	}
	candidates = dedupeItems(candidates)

	selected := s.selectByMode(candidates, cfg, userID, now)
	selected = backfillItems(selected, candidates, cfg.Limit)
	selected = preferBackdrop(selected, cfg.Limit)

	items := make([]Item, 0, len(selected))
	for _, it := range selected {
		items = append(items, s.toFeaturedItem(it, played, cfg))
	}
	if cfg.Limit > 0 && len(items) > cfg.Limit {
		items = items[:cfg.Limit]
	}
	return items, nil
}

func (s *Service) relaxCandidates(ctx context.Context, all []media.Item, cfg Config, allowedRoots []string) []media.Item {
	candidates := make([]media.Item, 0, len(all))
	for _, item := range all {
		if !passesLibraryFilter(ctx, s, item, allowedRoots) {
			continue
		}
		candidates = append(candidates, item)
	}
	if len(candidates) == 0 {
		return all
	}
	return candidates
}

func (s *Service) selectByMode(candidates []media.Item, cfg Config, userID string, now time.Time) []media.Item {
	switch cfg.Mode {
	case ModeRecentReleases, ModeNewInPeriod:
		return selectRecent(candidates, cfg, now)
	case ModeFavorites:
		// Favorites table not available yet; fall back to recent.
		return selectRecent(candidates, cfg, now)
	case ModeCollections:
		return selectCollections(candidates, cfg, userID, now)
	case ModeRandomLibraries:
		return selectRandom(candidates, cfg, userID, now)
	default:
		return selectRandom(candidates, cfg, userID, now)
	}
}

func selectRecent(candidates []media.Item, cfg Config, now time.Time) []media.Item {
	type datedItem struct {
		item media.Item
		date time.Time
	}
	cutoff := time.Time{}
	if (cfg.Mode == ModeNewInPeriod || cfg.Mode == ModeRecentReleases) && cfg.PeriodDays > 0 {
		cutoff = now.AddDate(0, 0, -cfg.PeriodDays)
	}
	out := make([]datedItem, 0, len(candidates))
	for _, it := range candidates {
		date := relevantDate(it)
		if !cutoff.IsZero() && date.Before(cutoff) {
			continue
		}
		out = append(out, datedItem{item: it, date: date})
	}
	if len(out) == 0 {
		for _, it := range candidates {
			out = append(out, datedItem{item: it, date: it.CreatedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].date.After(out[j].date)
	})
	limit := cfg.Limit
	if limit <= 0 || limit > len(out) {
		limit = len(out)
	}
	selected := make([]media.Item, 0, limit)
	for i := 0; i < limit; i++ {
		selected = append(selected, out[i].item)
	}
	return selected
}

func selectCollections(candidates []media.Item, cfg Config, userID string, now time.Time) []media.Item {
	collections := make([]media.Item, 0, len(candidates))
	for _, it := range candidates {
		if strings.EqualFold(it.Type, "collection") {
			collections = append(collections, it)
		}
	}
	if len(collections) > 0 {
		return selectRandom(collections, cfg, userID, now)
	}
	return selectRandom(candidates, cfg, userID, now)
}

func selectRandom(candidates []media.Item, cfg Config, userID string, now time.Time) []media.Item {
	if len(candidates) == 0 {
		return []media.Item{}
	}
	seed := randomSeed(cfg, userID, now)
	rng := rand.New(rand.NewSource(seed))
	shuffled := append([]media.Item{}, candidates...)
	rng.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	if cfg.Limit > 0 && len(shuffled) > cfg.Limit {
		return shuffled[:cfg.Limit]
	}
	return shuffled
}

func preferBackdrop(items []media.Item, limit int) []media.Item {
	if len(items) == 0 {
		return items
	}
	withBackdrop := make([]media.Item, 0, len(items))
	withoutBackdrop := make([]media.Item, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.BackdropURL) != "" {
			withBackdrop = append(withBackdrop, it)
		} else {
			withoutBackdrop = append(withoutBackdrop, it)
		}
	}
	if len(withBackdrop) >= limit && limit > 0 {
		return withBackdrop[:limit]
	}
	out := append(withBackdrop, withoutBackdrop...)
	if limit > 0 && len(out) > limit {
		return out[:limit]
	}
	return out
}

func (s *Service) toFeaturedItem(it media.Item, played map[string]int64, cfg Config) Item {
	meta := it.Metadata
	title := displayTitle(it)
	overview := firstNonEmpty(meta["plot"], meta["overview"], meta["tagline"])
	year := displayYear(it)
	runtime := parseInt(meta["runtime"])
	genres := parseGenres(meta)
	community, _ := parseFloat(meta, "rating", "vote_average")
	critic := parseInt(firstNonEmpty(meta["critic_rating"], meta["metacritic"]))
	parental := firstNonEmpty(meta["parental_rating"], meta["certification"], meta["mpaa"])
	isPlayed := played[it.ID] > 0
	backdrop := it.BackdropURL
	primary := it.PosterURL
	if cfg.ImageResizeEnabled {
		backdrop = resizeImage(backdrop, "backdrop")
		primary = resizeImage(primary, "poster")
	}
	if backdrop == "" {
		backdrop = primary
	}
	return Item{
		ID:       it.ID,
		Type:     firstNonEmpty(it.Type, meta["type"]),
		Title:    title,
		Overview: overview,
		Year:     year,
		RuntimeMinutes: runtime,
		Genres:   genres,
		CommunityRating: community,
		CriticRating:    critic,
		ParentalRating:  parental,
		IsPlayed:        isPlayed,
		Images: Images{
			Backdrop: backdrop,
			Logo:     meta["logo_url"],
			Primary:  primary,
		},
		Actions: Actions{
			PlayURL:    "/stream/session?mediaId=" + it.ID,
			DetailsURL: "/media/" + it.ID,
		},
		Badges: buildBadges(it),
	}
}

func (s *Service) listItems(ctx context.Context) ([]media.Item, error) {
	iter := s.session.Query(fmt.Sprintf(`SELECT id,type,title,year,season,episode,show_id,metadata,poster_url,backdrop_url,created_at FROM %s.media_items`, s.keyspace)).
		WithContext(ctx).
		Iter()
	items := make([]media.Item, 0)
	var it media.Item
	for iter.Scan(&it.ID, &it.Type, &it.Title, &it.Year, &it.Season, &it.Episode, &it.ShowID, &it.Metadata, &it.PosterURL, &it.BackdropURL, &it.CreatedAt) {
		items = append(items, it)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Service) listPlayState(ctx context.Context, userID string) (map[string]int64, error) {
	if strings.TrimSpace(userID) == "" {
		return map[string]int64{}, nil
	}
	uid, err := gocql.ParseUUID(userID)
	if err != nil {
		return map[string]int64{}, nil
	}
	iter := s.session.Query(fmt.Sprintf(`SELECT media_id, position_ms FROM %s.play_state WHERE user_id=?`, s.keyspace), uid).
		WithContext(ctx).
		Iter()
	out := make(map[string]int64)
	var mediaID gocql.UUID
	var pos int64
	for iter.Scan(&mediaID, &pos) {
		out[mediaID.String()] = pos
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) loadLibraries(ctx context.Context) ([]db.Library, error) {
	return db.ListLibraries(ctx, s.session, s.keyspace)
}

func buildAllowedRoots(cfg Config, libs []db.Library) []string {
	if len(cfg.LibrariesAllowed) == 0 || len(libs) == 0 {
		return nil
	}
	allowed := make(map[string]db.Library, len(cfg.LibrariesAllowed))
	for _, lib := range libs {
		allowed[lib.ID] = lib
	}
	var roots []string
	for _, id := range cfg.LibrariesAllowed {
		lib, ok := allowed[id]
		if !ok {
			continue
		}
		if strings.TrimSpace(lib.Path) != "" {
			roots = append(roots, lib.Path)
		}
	}
	return roots
}

func passesLibraryFilter(ctx context.Context, svc *Service, item media.Item, allowedRoots []string) bool {
	if len(allowedRoots) == 0 {
		return true
	}
	path, err := svc.firstAssetPath(ctx, item.ID)
	if err != nil || path == "" {
		return false
	}
	return isWithinRoots(path, allowedRoots)
}

func (s *Service) firstAssetPath(ctx context.Context, mediaID string) (string, error) {
	uid, err := gocql.ParseUUID(mediaID)
	if err != nil {
		return "", err
	}
	var path string
	err = s.session.Query(fmt.Sprintf(`SELECT path FROM %s.media_assets WHERE media_id=? LIMIT 1`, s.keyspace), uid).
		WithContext(ctx).
		Scan(&path)
	if err != nil {
		return "", err
	}
	return path, nil
}

func isWithinRoots(path string, roots []string) bool {
	clean := strings.ToLower(strings.TrimSpace(path))
	for _, root := range roots {
		r := strings.ToLower(strings.TrimSpace(root))
		if r == "" {
			continue
		}
		if strings.HasPrefix(clean, strings.ToLower(r)) {
			return true
		}
	}
	return false
}

func passesParentalFilter(item media.Item, cfg Config) bool {
	meta := item.Metadata
	rating := firstNonEmpty(meta["parental_rating"], meta["certification"], meta["mpaa"])
	if len(cfg.ParentalRatingAllowed) > 0 {
		if rating == "" {
			return true
		}
		for _, allowed := range cfg.ParentalRatingAllowed {
			if strings.EqualFold(strings.TrimSpace(allowed), rating) {
				return true
			}
		}
		return false
	}
	if cfg.MaxParentalRating == "" || rating == "" {
		return true
	}
	maxVal, maxOK := parseParentalNumeric(cfg.MaxParentalRating)
	val, ok := parseParentalNumeric(rating)
	if maxOK && ok {
		return val <= maxVal
	}
	return true
}

func parseParentalNumeric(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	if v, err := strconv.Atoi(trimmed); err == nil {
		return v, true
	}
	return 0, false
}

func passesRatingFilter(item media.Item, cfg Config) bool {
	meta := item.Metadata
	community, hasCommunity := parseFloat(meta, "rating", "vote_average")
	if cfg.MinCommunityRating > 0 {
		if !hasCommunity || community < cfg.MinCommunityRating {
			return false
		}
	}
	criticRaw := firstNonEmpty(meta["critic_rating"], meta["metacritic"])
	critic, hasCritic := parseIntValue(criticRaw)
	if cfg.MinCriticRating > 0 {
		if !hasCritic || critic < cfg.MinCriticRating {
			return false
		}
	}
	return true
}

func displayTitle(it media.Item) string {
	meta := it.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	if v := strings.TrimSpace(meta["title"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(meta["original_title"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(meta["name"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(meta["original_name"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(it.Title); v != "" {
		return v
	}
	return "Sans titre"
}

func displayYear(it media.Item) int {
	if it.Year > 0 {
		return it.Year
	}
	meta := it.Metadata
	if meta == nil {
		return 0
	}
	if v, err := strconv.Atoi(strings.TrimSpace(meta["year"])); err == nil {
		return v
	}
	return 0
}

func parseGenres(meta map[string]string) []string {
	if meta == nil {
		return nil
	}
	if raw := strings.TrimSpace(meta["genres_json"]); raw != "" {
		var out []string
		if err := json.Unmarshal([]byte(raw), &out); err == nil {
			return out
		}
	}
	if raw := strings.TrimSpace(meta["genres"]); raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return nil
}

func parseInt(raw string) int {
	v, _ := parseIntValue(raw)
	return v
}

func parseIntValue(raw string) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseFloat(meta map[string]string, keys ...string) (float64, bool) {
	if meta == nil {
		return 0, false
	}
	for _, key := range keys {
		if raw := strings.TrimSpace(meta[key]); raw != "" {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func buildBadges(it media.Item) []string {
	meta := it.Metadata
	source := ""
	if meta != nil {
		source = meta["source_name"]
	}
	text := strings.ToLower(source)
	badges := []string{}
	switch {
	case strings.Contains(text, "2160") || strings.Contains(text, "4k") || strings.Contains(text, "uhd"):
		badges = append(badges, "4K")
	case strings.Contains(text, "1080"):
		badges = append(badges, "1080p")
	case strings.Contains(text, "720"):
		badges = append(badges, "720p")
	}
	if strings.Contains(text, "hdr") {
		badges = append(badges, "HDR")
	}
	if strings.Contains(text, "dolby") {
		badges = append(badges, "Dolby")
	}
	if strings.Contains(text, "atmos") {
		badges = append(badges, "Atmos")
	}
	if strings.EqualFold(it.Type, "series") {
		badges = append(badges, "Serie")
	}
	return badges
}

func relevantDate(it media.Item) time.Time {
	meta := it.Metadata
	if strings.EqualFold(it.Type, "series") {
		if t := parseDate(meta, "end_date", "last_air_date", "last_airdate", "last_air"); !t.IsZero() {
			return t
		}
		if !it.CreatedAt.IsZero() {
			return it.CreatedAt
		}
	} else {
		if t := parseDate(meta, "release_date"); !t.IsZero() {
			return t
		}
	}
	if !it.CreatedAt.IsZero() {
		return it.CreatedAt
	}
	if it.Year > 0 {
		return time.Date(it.Year, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

func parseDate(meta map[string]string, keys ...string) time.Time {
	if meta == nil {
		return time.Time{}
	}
	for _, key := range keys {
		if raw := strings.TrimSpace(meta[key]); raw != "" {
			if t, err := time.Parse("2006-01-02", raw); err == nil {
				return t
			}
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return t
			}
			if len(raw) == 4 {
				if year, err := strconv.Atoi(raw); err == nil {
					return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
				}
			}
		}
	}
	return time.Time{}
}

func randomSeed(cfg Config, userID string, now time.Time) int64 {
	base := userID
	if strings.EqualFold(cfg.RandomSeedStrategy, RandomDailyPerUser) {
		base = base + ":" + now.UTC().Format("2006-01-02")
	} else {
		base = base + ":" + fmt.Sprintf("%d", now.UnixNano())
	}
	sum := sha256.Sum256([]byte(base))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func isRandomMode(mode Mode) bool {
	return mode == ModeRandomLibraries
}

func resizeImage(url, kind string) string {
	if strings.TrimSpace(url) == "" {
		return ""
	}
	if strings.Contains(url, "image.tmdb.org") {
		if kind == "backdrop" {
			return strings.Replace(url, "/w1280/", "/w780/", 1)
		}
		if kind == "poster" {
			return strings.Replace(url, "/w500/", "/w342/", 1)
		}
	}
	return url
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func dedupeItems(items []media.Item) []media.Item {
	seen := make(map[string]bool, len(items))
	out := make([]media.Item, 0, len(items))
	for _, it := range items {
		if it.ID == "" || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		out = append(out, it)
	}
	return out
}

func backfillItems(selected, candidates []media.Item, limit int) []media.Item {
	if limit <= 0 || len(selected) >= limit {
		return selected
	}
	seen := make(map[string]bool, len(selected))
	out := make([]media.Item, 0, limit)
	for _, it := range selected {
		if it.ID == "" || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		out = append(out, it)
	}
	for _, it := range candidates {
		if len(out) >= limit {
			break
		}
		if it.ID == "" || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		out = append(out, it)
	}
	return out
}
