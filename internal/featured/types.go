package featured

type Images struct {
	Backdrop string `json:"backdrop"`
	Logo     string `json:"logo"`
	Primary  string `json:"primary"`
}

type Actions struct {
	PlayURL    string `json:"playUrl"`
	DetailsURL string `json:"detailsUrl"`
}

type Item struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Title           string   `json:"title"`
	Overview        string   `json:"overview"`
	Year            int      `json:"year,omitempty"`
	RuntimeMinutes  int      `json:"runtimeMinutes,omitempty"`
	Genres          []string `json:"genres,omitempty"`
	CommunityRating float64  `json:"communityRating,omitempty"`
	CriticRating    int      `json:"criticRating,omitempty"`
	ParentalRating  string   `json:"parentalRating,omitempty"`
	IsPlayed        bool     `json:"isPlayed"`
	Images          Images   `json:"images"`
	Actions         Actions  `json:"actions"`
	Badges          []string `json:"badges,omitempty"`
}

type PublicConfig struct {
	Mode                  Mode     `json:"mode"`
	Limit                 int      `json:"limit"`
	LibrariesAllowed      []string `json:"librariesAllowed,omitempty"`
	ExcludePlayed         bool     `json:"excludePlayed"`
	MinCommunityRating    float64  `json:"minCommunityRating"`
	MinCriticRating       int      `json:"minCriticRating"`
	ParentalRatingAllowed []string `json:"parentalRatingAllowed,omitempty"`
	MaxParentalRating     string   `json:"maxParentalRating,omitempty"`
	RandomSeedStrategy    string   `json:"randomSeedStrategy,omitempty"`
	PeriodDays            int      `json:"periodDays"`
	ImageResizeEnabled    bool     `json:"imageResizeEnabled"`
	UI                    UIConfig `json:"ui"`
}

type ItemsResponse struct {
	Items  []Item       `json:"items"`
	Config PublicConfig `json:"config"`
}

func (c Config) Public() PublicConfig {
	return PublicConfig{
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
		UI:                    c.UI,
	}
}
