package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	tmdbBase   = "https://api.themoviedb.org/3"
	httpClient = &http.Client{Timeout: 5 * time.Second}
	errNoKey   = errors.New("TMDB_API_KEY no está configurada en el servidor")
)

const (
	imgPoster   = "https://image.tmdb.org/t/p/w342"
	imgBackdrop = "https://image.tmdb.org/t/p/w780"
	imgLogo     = "https://image.tmdb.org/t/p/w92"
)

func tmdbGet(ctx context.Context, path string, params url.Values, out any) error {
	key := strings.TrimSpace(os.Getenv("TMDB_API_KEY"))
	if key == "" {
		return errNoKey
	}
	if params == nil {
		params = url.Values{}
	}
	bearer := len(key) > 40 // token v4
	if !bearer {
		params.Set("api_key", key)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmdbBase+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	if bearer {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tmdb respondió %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type tmdbItem struct {
	ID            int        `json:"id"`
	MediaType     string     `json:"media_type"`
	Title         string     `json:"title"`
	Name          string     `json:"name"`
	OriginalTitle string     `json:"original_title"`
	OriginalName  string     `json:"original_name"`
	Overview      string     `json:"overview"`
	PosterPath    string     `json:"poster_path"`
	BackdropPath  string     `json:"backdrop_path"`
	ReleaseDate   string     `json:"release_date"`
	FirstAirDate  string     `json:"first_air_date"`
	VoteAverage   float64    `json:"vote_average"`
	VoteCount     int        `json:"vote_count"`
	Popularity    float64    `json:"popularity"`
	KnownFor      []tmdbItem `json:"known_for"`
}

func (i tmdbItem) title() string {
	if i.Title != "" {
		return i.Title
	}
	return i.Name
}

func (i tmdbItem) orig() string {
	if i.OriginalTitle != "" {
		return i.OriginalTitle
	}
	return i.OriginalName
}

func (i tmdbItem) date() string {
	if i.ReleaseDate != "" {
		return i.ReleaseDate
	}
	return i.FirstAirDate
}

type candidate struct {
	ID                         int
	Type                       string
	Title, OrigTitle, EnTitle  string
	Overview, Poster, Backdrop string
	Date                       string
	Rating, Pop                float64
	Votes                      int
	Via                        string
	viaScore                   float64
	hasEsTitle, hasEsOverview  bool
	direct                     bool
}

func img(base, p string) string {
	if p == "" {
		return ""
	}
	return base + p
}

func addItems(m map[string]*candidate, items []tmdbItem, lang, via string, viaScore float64) {
	for _, it := range items {
		if it.MediaType != "movie" && it.MediaType != "tv" {
			continue
		}
		if it.title() == "" {
			continue
		}
		k := fmt.Sprintf("%s:%d", it.MediaType, it.ID)
		c, ok := m[k]
		if !ok {
			c = &candidate{ID: it.ID, Type: it.MediaType}
			m[k] = c
		}
		if via == "" {
			c.direct, c.Via = true, "" // coincidencia directa gana sobre "por actor"
		} else if !c.direct && c.Via == "" {
			c.Via, c.viaScore = via, viaScore
		}
		c.OrigTitle = it.orig()
		if lang == "es-ES" {
			c.Title, c.hasEsTitle = it.title(), true
			if it.Overview != "" {
				c.Overview, c.hasEsOverview = it.Overview, true
			}
		} else {
			c.EnTitle = it.title()
			if !c.hasEsTitle {
				c.Title = it.title()
			}
			if !c.hasEsOverview && it.Overview != "" {
				c.Overview = it.Overview
			}
		}
		if c.Poster == "" {
			c.Poster = img(imgPoster, it.PosterPath)
		}
		if c.Backdrop == "" {
			c.Backdrop = img(imgBackdrop, it.BackdropPath)
		}
		if c.Date == "" {
			c.Date = it.date()
		}
		c.Rating, c.Votes = it.VoteAverage, it.VoteCount
		if it.Popularity > c.Pop {
			c.Pop = it.Popularity
		}
	}
}

// fetchCandidates consulta TMDB en paralelo (es-ES y en-US) y fusiona los resultados.
func fetchCandidates(ctx context.Context, queries []string) ([]*candidate, error) {
	type job struct{ q, lang string }
	var jobs []job
	for _, q := range queries {
		jobs = append(jobs, job{q, "es-ES"}, job{q, "en-US"})
	}
	type jobRes struct {
		items []tmdbItem
		err   error
	}
	res := make([]jobRes, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			var out struct {
				Results []tmdbItem `json:"results"`
			}
			err := tmdbGet(ctx, "/search/multi", url.Values{
				"query": {j.q}, "language": {j.lang}, "include_adult": {"false"}, "page": {"1"},
			}, &out)
			res[i] = jobRes{out.Results, err}
		}(i, j)
	}
	wg.Wait()

	m := map[string]*candidate{}
	var firstErr error
	ok := false
	for i, r := range res {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		ok = true
		lang := jobs[i].lang
		qf := fold(jobs[i].q)
		var direct []tmdbItem
		for _, it := range r.items {
			if it.MediaType == "person" {
				// Si buscan a un actor/director, muestra sus obras más conocidas.
				if s := textScore(qf, fold(it.Name)); s >= 70 {
					addItems(m, it.KnownFor, lang, it.Name, s)
				}
				continue
			}
			direct = append(direct, it)
		}
		addItems(m, direct, lang, "", 0)
	}
	if !ok {
		return nil, firstErr
	}
	out := make([]*candidate, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	return out, nil
}

// ---------- Detalle de un título ----------

type Provider struct {
	Name string `json:"name"`
	Logo string `json:"logo,omitempty"`
	Kind string `json:"kind"` // flatrate | free | ads | rent | buy
}

type Details struct {
	ID        int        `json:"id"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Tagline   string     `json:"tagline,omitempty"`
	Overview  string     `json:"overview,omitempty"`
	Year      string     `json:"year,omitempty"`
	Runtime   int        `json:"runtime,omitempty"`
	Seasons   int        `json:"seasons,omitempty"`
	Genres    []string   `json:"genres,omitempty"`
	Rating    float64    `json:"rating,omitempty"`
	Poster    string     `json:"poster,omitempty"`
	Backdrop  string     `json:"backdrop,omitempty"`
	Trailer   string     `json:"trailer,omitempty"`
	Country   string     `json:"country"`
	WatchLink string     `json:"watchLink,omitempty"`
	Providers []Provider `json:"providers"`
}

type tmdbProv struct {
	Name string `json:"provider_name"`
	Logo string `json:"logo_path"`
}

func fetchDetails(ctx context.Context, typ string, id int, country string) (*Details, error) {
	var d struct {
		tmdbItem
		Tagline        string `json:"tagline"`
		Runtime        int    `json:"runtime"`
		EpisodeRunTime []int  `json:"episode_run_time"`
		Seasons        int    `json:"number_of_seasons"`
		Genres         []struct {
			Name string `json:"name"`
		} `json:"genres"`
		Videos struct {
			Results []struct {
				Key      string `json:"key"`
				Site     string `json:"site"`
				Type     string `json:"type"`
				Official bool   `json:"official"`
			} `json:"results"`
		} `json:"videos"`
		WP struct {
			Results map[string]struct {
				Link     string     `json:"link"`
				Flatrate []tmdbProv `json:"flatrate"`
				Free     []tmdbProv `json:"free"`
				Ads      []tmdbProv `json:"ads"`
				Rent     []tmdbProv `json:"rent"`
				Buy      []tmdbProv `json:"buy"`
			} `json:"results"`
		} `json:"watch/providers"`
	}
	err := tmdbGet(ctx, fmt.Sprintf("/%s/%d", typ, id), url.Values{
		"language":               {"es-ES"},
		"append_to_response":     {"videos,watch/providers"},
		"include_video_language": {"es,en,null"},
	}, &d)
	if err != nil {
		return nil, err
	}
	out := &Details{
		ID: id, Type: typ, Title: d.title(), Tagline: d.Tagline, Overview: d.Overview,
		Year: yearOf(d.date()), Runtime: d.Runtime, Seasons: d.Seasons,
		Rating: d.VoteAverage, Poster: img(imgPoster, d.PosterPath),
		Backdrop: img(imgBackdrop, d.BackdropPath), Country: country, Providers: []Provider{},
	}
	if out.Runtime == 0 && len(d.EpisodeRunTime) > 0 {
		out.Runtime = d.EpisodeRunTime[0]
	}
	for _, g := range d.Genres {
		out.Genres = append(out.Genres, g.Name)
	}
	// Mejor tráiler: oficial > cualquiera > teaser (solo YouTube).
	bestRank := -1
	for _, v := range d.Videos.Results {
		if v.Site != "YouTube" || v.Key == "" {
			continue
		}
		r := 0
		if v.Type == "Trailer" {
			r = 2
			if v.Official {
				r = 3
			}
		} else if v.Type == "Teaser" {
			r = 1
		}
		if r > bestRank {
			bestRank = r
			out.Trailer = "https://www.youtube.com/watch?v=" + url.QueryEscape(v.Key)
		}
	}
	if wp, ok := d.WP.Results[country]; ok {
		out.WatchLink = wp.Link
		add := func(list []tmdbProv, kind string) {
			for _, p := range list {
				out.Providers = append(out.Providers, Provider{p.Name, img(imgLogo, p.Logo), kind})
			}
		}
		add(wp.Flatrate, "flatrate")
		add(wp.Free, "free")
		add(wp.Ads, "ads")
		add(wp.Rent, "rent")
		add(wp.Buy, "buy")
	}
	return out, nil
}
