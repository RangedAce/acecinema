package featured

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Example env config:
// FEATURED_MODE=RANDOM_FROM_LIBRARIES
// FEATURED_LIMIT=6
// FEATURED_LIBRARIES_ALLOWED=8c7b9e8a-...,3d5a...
// FEATURED_EXCLUDE_PLAYED=false
// FEATURED_MIN_COMMUNITY_RATING=0
// FEATURED_MIN_CRITIC_RATING=0
// FEATURED_PARENTAL_ALLOWED=G,PG-13
// FEATURED_RANDOM_SEED_STRATEGY=DAILY_PER_USER
// FEATURED_PERIOD_DAYS=60
// FEATURED_IMAGE_RESIZE=false
// FEATURED_CACHE_TTL_RANDOM_MINUTES=60
// FEATURED_CACHE_TTL_RECENT_MINUTES=10
// FEATURED_UI_AUTOPLAY=true
// FEATURED_UI_AUTOPLAY_INTERVAL_MS=7000
// FEATURED_UI_LOOP=true
// FEATURED_UI_SHOW_OVERVIEW=true
// FEATURED_UI_SHOW_RATINGS=true
// FEATURED_UI_HEIGHT=360px
// FEATURED_UI_TV_LAYOUT=false
// FEATURED_UI_HIDE_ON_TV_LAYOUT=false
type Config struct {
	Mode                 Mode
	Limit                int
	LibrariesAllowed     []string
	ExcludePlayed        bool
	MinCommunityRating   float64
	MinCriticRating      int
	ParentalRatingAllowed []string
	MaxParentalRating    string
	RandomSeedStrategy   string
	PeriodDays           int
	ImageResizeEnabled   bool
	CacheTTLRandom       time.Duration
	CacheTTLRecent       time.Duration
	UI                   UIConfig
}

type UIConfig struct {
	Autoplay           bool
	AutoplayIntervalMs int
	Loop               bool
	ShowOverview       bool
	ShowRatings        bool
	Height             string
	TVLayout           bool
	HideOnTVLayout     bool
}

type Mode string

const (
	ModeFavorites        Mode = "FAVORITES_OF_USER"
	ModeRandomLibraries  Mode = "RANDOM_FROM_LIBRARIES"
	ModeRecentReleases   Mode = "RECENT_RELEASES"
	ModeCollections      Mode = "COLLECTIONS"
	ModeNewInPeriod      Mode = "NEW_IN_PERIOD"
	RandomDailyPerUser        = "DAILY_PER_USER"
)

func DefaultConfig() Config {
	return Config{
		Mode:               ModeRandomLibraries,
		Limit:              6,
		ExcludePlayed:      false,
		MinCommunityRating: 0,
		MinCriticRating:    0,
		RandomSeedStrategy: RandomDailyPerUser,
		PeriodDays:         60,
		ImageResizeEnabled: false,
		CacheTTLRandom:     time.Hour,
		CacheTTLRecent:     10 * time.Minute,
		UI: UIConfig{
			Autoplay:           true,
			AutoplayIntervalMs: 7000,
			Loop:               true,
			ShowOverview:       true,
			ShowRatings:        true,
			Height:             "",
			TVLayout:           false,
			HideOnTVLayout:     false,
		},
	}
}

func LoadConfigFromEnv() Config {
	cfg := DefaultConfig()

	if v := strings.TrimSpace(os.Getenv("FEATURED_MODE")); v != "" {
		cfg.Mode = normalizeMode(v, cfg.Mode)
	}
	if v := strings.TrimSpace(os.Getenv("FEATURED_LIMIT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Limit = n
		}
	}
	if v := os.Getenv("FEATURED_LIBRARIES_ALLOWED"); v != "" {
		cfg.LibrariesAllowed = splitCSV(v)
	}
	if v := os.Getenv("FEATURED_EXCLUDE_PLAYED"); v != "" {
		cfg.ExcludePlayed = parseBool(v, cfg.ExcludePlayed)
	}
	if v := os.Getenv("FEATURED_MIN_COMMUNITY_RATING"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			cfg.MinCommunityRating = f
		}
	}
	if v := os.Getenv("FEATURED_MIN_CRITIC_RATING"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.MinCriticRating = n
		}
	}
	if v := os.Getenv("FEATURED_PARENTAL_ALLOWED"); v != "" {
		cfg.ParentalRatingAllowed = splitCSV(v)
	}
	if v := os.Getenv("FEATURED_MAX_PARENTAL_RATING"); v != "" {
		cfg.MaxParentalRating = strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(os.Getenv("FEATURED_RANDOM_SEED_STRATEGY")); v != "" {
		cfg.RandomSeedStrategy = v
	}
	if v := os.Getenv("FEATURED_PERIOD_DAYS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.PeriodDays = n
		}
	}
	if v := os.Getenv("FEATURED_IMAGE_RESIZE"); v != "" {
		cfg.ImageResizeEnabled = parseBool(v, cfg.ImageResizeEnabled)
	}
	if v := os.Getenv("FEATURED_CACHE_TTL_RANDOM_MINUTES"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.CacheTTLRandom = time.Duration(n) * time.Minute
		}
	}
	if v := os.Getenv("FEATURED_CACHE_TTL_RECENT_MINUTES"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.CacheTTLRecent = time.Duration(n) * time.Minute
		}
	}
	if v := os.Getenv("FEATURED_UI_AUTOPLAY"); v != "" {
		cfg.UI.Autoplay = parseBool(v, cfg.UI.Autoplay)
	}
	if v := os.Getenv("FEATURED_UI_AUTOPLAY_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.UI.AutoplayIntervalMs = n
		}
	}
	if v := os.Getenv("FEATURED_UI_LOOP"); v != "" {
		cfg.UI.Loop = parseBool(v, cfg.UI.Loop)
	}
	if v := os.Getenv("FEATURED_UI_SHOW_OVERVIEW"); v != "" {
		cfg.UI.ShowOverview = parseBool(v, cfg.UI.ShowOverview)
	}
	if v := os.Getenv("FEATURED_UI_SHOW_RATINGS"); v != "" {
		cfg.UI.ShowRatings = parseBool(v, cfg.UI.ShowRatings)
	}
	if v := strings.TrimSpace(os.Getenv("FEATURED_UI_HEIGHT")); v != "" {
		cfg.UI.Height = v
	}
	if v := os.Getenv("FEATURED_UI_TV_LAYOUT"); v != "" {
		cfg.UI.TVLayout = parseBool(v, cfg.UI.TVLayout)
	}
	if v := os.Getenv("FEATURED_UI_HIDE_ON_TV_LAYOUT"); v != "" {
		cfg.UI.HideOnTVLayout = parseBool(v, cfg.UI.HideOnTVLayout)
	}

	return cfg.normalize()
}

func (c Config) normalize() Config {
	if c.Mode == "" {
		c.Mode = ModeRandomLibraries
	}
	if c.Limit <= 0 {
		c.Limit = 6
	}
	if c.PeriodDays <= 0 {
		c.PeriodDays = 60
	}
	if c.CacheTTLRandom <= 0 {
		c.CacheTTLRandom = time.Hour
	}
	if c.CacheTTLRecent <= 0 {
		c.CacheTTLRecent = 10 * time.Minute
	}
	if c.UI.AutoplayIntervalMs <= 0 {
		c.UI.AutoplayIntervalMs = 7000
	}
	if c.RandomSeedStrategy == "" {
		c.RandomSeedStrategy = RandomDailyPerUser
	}
	return c
}

func normalizeMode(raw string, fallback Mode) Mode {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case string(ModeFavorites):
		return ModeFavorites
	case string(ModeRandomLibraries):
		return ModeRandomLibraries
	case string(ModeRecentReleases):
		return ModeRecentReleases
	case string(ModeCollections):
		return ModeCollections
	case string(ModeNewInPeriod):
		return ModeNewInPeriod
	default:
		return fallback
	}
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseBool(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}
