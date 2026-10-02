package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	tmdbBase   = "https://api.themoviedb.org/3"
	httpClient = &http.Client{Timeout: 4 * time.Second}
	errNoKey   = errors.New("TMDB_API_KEY no está configurada en el servidor")
	errRate    = errors.New("TMDB está limitando las peticiones")
)

const (
	imgPoster   = "https://image.tmdb.org/t/p/w342"
	imgBackdrop = "https://image.tmdb.org/t/p/w780"
	imgLogo     = "https://image.tmdb.org/t/p/w92"

	maxCredits = 30
)

// tmdbGet consulta la API de TMDB y reintenta una vez si hay 429 o error 5xx.
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
	endpoint := tmdbBase + path + "?" + params.Encode()

	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			switch {
			case resp.StatusCode == http.StatusTooManyRequests:
				last = fmt.Errorf("%w (%d)", errRate, resp.StatusCode)
			case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
				last = errNoKey
			default:
				last = fmt.Errorf("tmdb respondió %d", resp.StatusCode)
			}
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				continue // vale la pena reintentar una vez
			}
			return last
		}
		if readErr != nil {
			last = readErr
			continue
		}
		return json.Unmarshal(body, out)
	}
	return last
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
	GenreIDs      []int      `json:"genre_ids"`
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
	hasEs                      bool
	direct                     bool
}

// plausibleCredit descarta apariciones que no explican la búsqueda: un papel
// pequeño con casi ningún voto no es lo que alguien busca al pedir un actor.
func plausibleCredit(it tmdbItem) bool {
	if it.title() == "" {
		return false
	}
	if it.VoteCount >= 50 || it.Popularity >= 5 {
		return true
	}
	return false
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
			c.Title, c.hasEs = it.title(), true
			if it.Overview != "" {
				c.Overview = it.Overview
			}
		} else {
			c.EnTitle = it.title()
			if !c.hasEs {
				c.Title = it.title()
			}
			if c.Overview == "" && it.Overview != "" {
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

// personHit es el actor/director que mejor respondió a la consulta.
type personHit struct {
	ID    int
	Name  string
	Score float64
}

// searchTMDB consulta /search/multi en paralelo y fusiona los resultados.
// Devuelve los candidatos y, si la consulta era el nombre de una persona, esa
// persona para poder traer su filmografía completa.
func searchTMDB(ctx context.Context, queries, langs []string) (map[string]*candidate, *personHit, error) {
	type job struct{ q, lang string }
	var jobs []job
	for _, lang := range langs {
		for _, q := range queries {
			jobs = append(jobs, job{q, lang})
		}
	}
	if len(jobs) == 0 {
		return map[string]*candidate{}, nil, nil
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
				"query": {j.q}, "language": {j.lang}, "include_adult": {"false"},
				"page": {"1"}, "include_video": {"false"},
			}, &out)
			res[i] = jobRes{out.Results, err}
		}(i, j)
	}
	wg.Wait()

	m := map[string]*candidate{}
	var hit *personHit
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
				// Si buscan a un actor/director, guarda sus obras más conocidas.
				if s := textScore(qf, fold(it.Name)); s >= 70 {
					var known []tmdbItem
					for _, k := range it.KnownFor {
						if plausibleCredit(k) {
							known = append(known, k)
						}
					}
					addItems(m, known, lang, it.Name, s)
					if hit == nil || s > hit.Score {
						hit = &personHit{ID: it.ID, Name: it.Name, Score: s}
					}
				}
				continue
			}
			direct = append(direct, it)
		}
		addItems(m, direct, lang, "", 0)
	}
	if !ok && firstErr != nil {
		return nil, nil, firstErr
	}
	return m, hit, nil
}

func mergeCandidates(dst, src map[string]*candidate) {
	for k, v := range src {
		if old, ok := dst[k]; ok {
			if old.Pop < v.Pop {
				old.Pop = v.Pop
			}
			continue
		}
		dst[k] = v
	}
}

func candidatesToSlice(m map[string]*candidate) []*candidate {
	out := make([]*candidate, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	return out
}

// strongCount cuenta candidatos que realmente responden a la consulta.
func strongCount(cands []*candidate, p parsed) int {
	n := 0
	for _, c := range cands {
		if bestText(p.Fold, c.Title, c.OrigTitle, c.EnTitle) >= 55 {
			n++
		}
	}
	return n
}

// fetchCandidates arma el conjunto de candidatos: primero la búsqueda directa en
// español e inglés y, solo si no encuentra nada decente, variantes para erratas,
// búsquedas a medio escribir y títulos con palabras de más.
func fetchCandidates(ctx context.Context, p parsed, quick bool) ([]*candidate, error) {
	m, hit, err := searchTMDB(ctx, p.primary(), []string{"es-ES", "en-US"})
	if m == nil {
		return nil, err
	}

	decent := strongCount(candidatesToSlice(m), p) >= 2 ||
		bestMediaText(m, p) >= 65 ||
		(hit != nil && hit.Score >= 85)

	if !quick && !decent {
		if vars := p.variants(); len(vars) > 0 {
			if extra, _, err2 := searchTMDB(ctx, vars, []string{"es-ES"}); err2 == nil {
				mergeCandidates(m, extra)
			}
		}
	}

	// La consulta era el nombre de alguien: su filmografía vale más que 4 obras.
	if !quick && hit != nil && hit.Score >= 85 && bestMediaText(m, p) < 55 {
		addPersonCredits(ctx, m, hit, p)
	}
	return candidatesToSlice(m), nil
}

func bestMediaText(m map[string]*candidate, p parsed) float64 {
	best := 0.0
	for _, c := range m {
		best = math.Max(best, bestText(p.Fold, c.Title, c.OrigTitle, c.EnTitle))
	}
	return best
}

// addPersonCredits suma la filmografía de una persona (no solo "known_for").
func addPersonCredits(ctx context.Context, m map[string]*candidate, hit *personHit, p parsed) {
	if hit == nil || hit.ID <= 0 {
		return
	}
	var d struct {
		MovieCredits struct {
			Cast []tmdbItem `json:"cast"`
		} `json:"movie_credits"`
		TVCredits struct {
			Cast []tmdbItem `json:"cast"`
		} `json:"tv_credits"`
	}
	if err := tmdbGet(ctx, fmt.Sprintf("/person/%d", hit.ID), url.Values{
		"language":           {"es-ES"},
		"append_to_response": {"movie_credits,tv_credits"},
	}, &d); err != nil {
		return // la búsqueda sigue siendo válida sin la filmografía completa
	}
	items := make([]tmdbItem, 0, len(d.MovieCredits.Cast)+len(d.TVCredits.Cast))
	for _, it := range d.MovieCredits.Cast {
		it.MediaType = "movie"
		items = append(items, it)
	}
	for _, it := range d.TVCredits.Cast {
		it.MediaType = "tv"
		items = append(items, it)
	}
	var good []tmdbItem
	for _, it := range items {
		if plausibleCredit(it) {
			good = append(good, it)
		}
	}
	sort.SliceStable(good, func(i, j int) bool {
		return good[i].VoteCount*2+int(good[i].Popularity) > good[j].VoteCount*2+int(good[j].Popularity)
	})
	if len(good) > maxCredits {
		good = good[:maxCredits]
	}
	addItems(m, good, "es-ES", hit.Name, hit.Score)
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
	Original  string     `json:"originalTitle,omitempty"`
	Tagline   string     `json:"tagline,omitempty"`
	Overview  string     `json:"overview,omitempty"`
	Year      string     `json:"year,omitempty"`
	Runtime   int        `json:"runtime,omitempty"`
	Seasons   int        `json:"seasons,omitempty"`
	Genres    []string   `json:"genres,omitempty"`
	Rating    float64    `json:"rating,omitempty"`
	Votes     int        `json:"votes,omitempty"`
	Poster    string     `json:"poster,omitempty"`
	Backdrop  string     `json:"backdrop,omitempty"`
	Trailer   string     `json:"trailer,omitempty"`
	TrailerID string     `json:"trailerId,omitempty"`
	Country   string     `json:"country"`
	WatchLink string     `json:"watchLink,omitempty"`
	Providers []Provider `json:"providers"`
	Sources   []Source   `json:"sources"`
}

type tmdbProv struct {
	Name string `json:"provider_name"`
	Logo string `json:"logo_path"`
}

type tmdbDetail struct {
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

func fetchDetails(ctx context.Context, typ string, id int, country string) (*Details, error) {
	lang := "es-ES"
	d, err := getDetail(ctx, typ, id, lang)
	if err != nil {
		return nil, err
	}
	if d.title() == "" {
		// Sin traducción al español: reintenta en inglés.
		if en, err2 := getDetail(ctx, typ, id, "en-US"); err2 == nil {
			*d = *en
		}
	}

	out := &Details{
		ID: id, Type: typ, Title: d.title(), Original: d.orig(), Tagline: d.Tagline,
		Overview: d.Overview, Year: yearOf(d.date()), Runtime: d.Runtime, Seasons: d.Seasons,
		Rating: d.VoteAverage, Votes: d.VoteCount, Poster: img(imgPoster, d.PosterPath),
		Backdrop: img(imgBackdrop, d.BackdropPath), Country: country, Providers: []Provider{},
	}
	if out.Title == "" {
		out.Title = out.Original
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
			out.TrailerID = v.Key
			out.Trailer = "https://www.youtube.com/watch?v=" + url.QueryEscape(v.Key)
		}
	}
	if wp, ok := d.WP.Results[country]; ok {
		out.WatchLink = wp.Link
		add := func(list []tmdbProv, kind string) {
			seen := map[string]bool{}
			for _, p := range list {
				if p.Name == "" || seen[p.Name] {
					continue
				}
				seen[p.Name] = true
				out.Providers = append(out.Providers, Provider{p.Name, img(imgLogo, p.Logo), kind})
			}
		}
		add(wp.Flatrate, "flatrate")
		add(wp.Free, "free")
		add(wp.Ads, "ads")
		add(wp.Rent, "rent")
		add(wp.Buy, "buy")
	}
	if len(out.Providers) == 0 {
		// Sin plataformas en ese país, muestra las de la región para no dejar vacío.
		for cc, wp := range d.WP.Results {
			if cc == country {
				continue
			}
			for _, p := range wp.Flatrate {
				out.Providers = append(out.Providers, Provider{p.Name, img(imgLogo, p.Logo), "flatrate"})
			}
			if len(out.Providers) > 0 {
				break
			}
		}
	}
	out.Sources = buildSources(out.Title, out.Original, out.Year, typ, out.TrailerID)
	return out, nil
}

func getDetail(ctx context.Context, typ string, id int, lang string) (*tmdbDetail, error) {
	d := &tmdbDetail{}
	err := tmdbGet(ctx, fmt.Sprintf("/%s/%d", typ, id), url.Values{
		"language":               {lang},
		"append_to_response":     {"videos,watch/providers"},
		"include_video_language": {"es,en,null"},
	}, d)
	if err != nil {
		return nil, err
	}
	return d, nil
}
