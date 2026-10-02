package main

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	imgPosterCard = "https://image.tmdb.org/t/p/w500"
	imgBackdropHD = "https://image.tmdb.org/t/p/w1280"

	browseDefLimit = 24
	browseMaxLimit = 48
	browseMaxPage  = 12
	browseMinYear  = 1888
	browseMaxYear  = 2100

	genreTTL  = 24 * time.Hour
	rowTTL    = 15 * time.Minute
	detailTTL = 30 * time.Minute
)

var (
	errNoGenres = errors.New("no se pudieron cargar los géneros")
	errFiltros  = errors.New("filtros inválidos")
)

// Card es la ficha ligera que pinta el catálogo.
type Card struct {
	ID       int     `json:"id"`
	Type     string  `json:"type"`
	Title    string  `json:"title"`
	Year     string  `json:"year,omitempty"`
	Rating   float64 `json:"rating,omitempty"`
	Overview string  `json:"overview,omitempty"`
	Poster   string  `json:"poster,omitempty"`
	Backdrop string  `json:"backdrop,omitempty"`
	Genre    string  `json:"genre,omitempty"`
}

type tmdbPage struct {
	Page       int        `json:"page"`
	Results    []tmdbItem `json:"results"`
	TotalPages int        `json:"total_pages"`
}

type BrowseResponse struct {
	Items []Card `json:"items"`
	Page  int    `json:"page"`
	Pages int    `json:"pages"`
	Total int    `json:"total"`
	Type  string `json:"type"`
	Sort  string `json:"sort"`
	Genre int    `json:"genre,omitempty"`
	Year  int    `json:"year,omitempty"`
}

// ---------- Caché de respuestas del catálogo ----------

type rcEntry struct {
	key  string
	body []byte
	at   time.Time
}

var (
	rcMu  sync.Mutex
	rcMap = map[string]*list.Element{}
	rcLRU = list.New()
)

const (
	rcMax      = 600
	rcStaleFor = 2 * time.Hour
)

func rcGet(key string, ttl time.Duration) ([]byte, bool) {
	rcMu.Lock()
	defer rcMu.Unlock()
	el, ok := rcMap[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*rcEntry)
	if time.Since(it.at) > ttl {
		return nil, false
	}
	rcLRU.MoveToFront(el)
	return it.body, true
}

// rcStale devuelve respuestas vencidas para no dejar al usuario sin nada cuando
// TMDB falla.
func rcStale(key string) ([]byte, bool) {
	rcMu.Lock()
	defer rcMu.Unlock()
	el, ok := rcMap[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*rcEntry)
	if time.Since(it.at) > rcStaleFor {
		rcLRU.Remove(el)
		delete(rcMap, it.key)
		return nil, false
	}
	rcLRU.MoveToFront(el)
	return it.body, true
}

func rcSet(key string, body []byte) {
	rcMu.Lock()
	defer rcMu.Unlock()
	if el, ok := rcMap[key]; ok {
		it := el.Value.(*rcEntry)
		it.body, it.at = body, time.Now()
		rcLRU.MoveToFront(el)
		return
	}
	if rcLRU.Len() >= rcMax {
		if back := rcLRU.Back(); back != nil {
			rcLRU.Remove(back)
			delete(rcMap, back.Value.(*rcEntry).key)
		}
	}
	rcMap[key] = rcLRU.PushFront(&rcEntry{key: key, body: body, at: time.Now()})
}

// ---------- Una sola petición en curso por clave ----------

type anyCall struct {
	done chan struct{}
	val  any
	err  error
}

var (
	anyMu sync.Mutex
	anyIn = map[string]*anyCall{}
)

func fetchOnce(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	anyMu.Lock()
	if c, ok := anyIn[key]; ok {
		anyMu.Unlock()
		select {
		case <-c.done:
			return c.val, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &anyCall{done: make(chan struct{})}
	anyIn[key] = c
	anyMu.Unlock()

	// El trabajo no depende del cliente que llegó primero: si se va, los demás
	// siguen esperando lo mismo.
	fctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	c.val, c.err = fn(fctx)
	cancel()
	close(c.done)

	anyMu.Lock()
	delete(anyIn, key)
	anyMu.Unlock()
	return c.val, c.err
}

// writeJSONBytes responde con un cuerpo ya serializado (respuesta cacheada).
func writeJSONBytes(w http.ResponseWriter, body []byte, cache string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, s-maxage=300, stale-while-revalidate=3600")
	if cache != "" {
		h.Set("X-Cache", cache)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// serveCached entrega la respuesta desde la caché o la construye una sola vez,
// aunque lleguen veinte peticiones a la vez.
func serveCached(w http.ResponseWriter, key string, ttl time.Duration, build func(context.Context) (any, error)) {
	if body, ok := rcGet(key, ttl); ok {
		writeJSONBytes(w, body, "HIT")
		return
	}
	v, err := fetchOnce(context.Background(), key, build)
	if err != nil {
		if body, ok := rcStale(key); ok {
			writeJSONBytes(w, body, "STALE")
			return
		}
		writeErr(w, err)
		return
	}
	body, err := json.Marshal(v)
	if err != nil {
		writeErr(w, err)
		return
	}
	rcSet(key, body)
	writeJSONBytes(w, body, "MISS")
}

// ---------- Géneros ----------

type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type GenresResponse struct {
	Movie []Genre `json:"movie"`
	TV    []Genre `json:"tv"`
}

type genreSet struct {
	at     time.Time
	movies map[int]string
	tv     map[int]string
}

var (
	genreMu   sync.Mutex
	genreData *genreSet
	genreBusy time.Time
)

// genreSets devuelve el catálogo de géneros, que cambia poquísimo. Si alguien lo
// está trayendo ya, se espera en vez de disparar otra petición a TMDB.
func genreSets(ctx context.Context) (*genreSet, error) {
	genreMu.Lock()
	if genreData != nil && time.Since(genreData.at) < genreTTL {
		g := genreData
		genreMu.Unlock()
		return g, nil
	}
	busy := genreBusy
	genreMu.Unlock()

	if !busy.IsZero() && time.Since(busy) < time.Minute {
		genreMu.Lock()
		g := genreData
		genreMu.Unlock()
		if g != nil {
			return g, nil
		}
		return nil, errNoGenres
	}

	genreMu.Lock()
	genreBusy = time.Now()
	genreMu.Unlock()

	g, err := loadGenres(ctx)

	genreMu.Lock()
	genreBusy = time.Time{}
	if err == nil {
		genreData = g
	}
	genreMu.Unlock()
	return g, err
}

func loadGenres(ctx context.Context) (*genreSet, error) {
	var (
		movies, tv map[int]string
		firstErr   error
		wg         sync.WaitGroup
	)
	for _, kind := range []string{"movie", "tv"} {
		wg.Add(1)
		go func(kind string) {
			defer wg.Done()
			var out struct {
				Genres []Genre `json:"genres"`
			}
			if err := tmdbGet(ctx, "/genre/"+kind+"/list", url.Values{"language": {"es-ES"}}, &out); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			m := make(map[int]string, len(out.Genres))
			for _, g := range out.Genres {
				if g.Name != "" {
					m[g.ID] = g.Name
				}
			}
			if kind == "movie" {
				movies = m
			} else {
				tv = m
			}
		}(kind)
	}
	wg.Wait()

	if len(movies) == 0 && len(tv) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, errNoGenres
	}
	return &genreSet{at: time.Now(), movies: movies, tv: tv}, nil
}

func sortedGenres(m map[int]string) []Genre {
	out := make([]Genre, 0, len(m))
	for id, name := range m {
		out = append(out, Genre{ID: id, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func genresHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	serveCached(w, "genres", genreTTL, func(ctx context.Context) (any, error) {
		g, err := genreSets(ctx)
		if err != nil {
			return nil, err
		}
		return GenresResponse{Movie: sortedGenres(g.movies), TV: sortedGenres(g.tv)}, nil
	})
}

// ---------- Fichas ----------

// toCards convierte resultados de TMDB en fichas. typ es el tipo que se sabe de
// antemano (descubrir y listados no devuelven media_type). backdrop se pide
// grande solo para las fichas que van a ocupar la portada.
func toCards(items []tmdbItem, g *genreSet, typ string, backdrop bool) []Card {
	out := make([]Card, 0, len(items))
	for _, it := range items {
		t := it.MediaType
		if t == "" {
			t = typ
		}
		if t == "" {
			if it.Title != "" {
				t = "movie"
			} else {
				t = "tv"
			}
		}
		if t != "movie" && t != "tv" {
			continue
		}
		title := it.title()
		if title == "" {
			continue
		}
		c := Card{
			ID: it.ID, Type: t, Title: title, Year: yearOf(it.date()),
			Rating: math.Round(it.VoteAverage*10) / 10,
			Poster: img(imgPosterCard, it.PosterPath),
		}
		// Una nota de tres votos no dice nada: la escondemos.
		if c.Rating > 0 && (c.Rating < 3 || it.VoteCount < 40) {
			c.Rating = 0
		}
		if it.Overview != "" {
			c.Overview = trimRunes(it.Overview, 240)
		}
		if g != nil {
			src := g.movies
			if t == "tv" {
				src = g.tv
			}
			for _, id := range it.GenreIDs {
				if name := src[id]; name != "" {
					c.Genre = trimRunes(name, 16)
					break
				}
			}
		}
		if backdrop {
			c.Backdrop = img(imgBackdropHD, it.BackdropPath)
		}
		out = append(out, c)
	}
	return out
}

// ---------- Explorar ----------

type browse struct {
	Type      string
	Sort      string
	Region    string
	Genre     int
	Year      int
	Page      int
	Limit     int
	Backdrops bool // pide los fondos grandes (solo para la portada)
}

func (b browse) key() string {
	return fmt.Sprintf("browse|%s|%s|%s|%d|%d|%d|%d|%t", b.Type, b.Sort, b.Region, b.Genre, b.Year, b.Page, b.Limit, b.Backdrops)
}

// endpoint arma la llamada de TMDB para un tipo concreto.
func (b browse) endpoint(typ string) (string, url.Values) {
	v := url.Values{
		"language":      {"es-ES"},
		"page":          {strconv.Itoa(b.Page)},
		"include_adult": {"false"},
	}
	if b.Genre == 0 && b.Year == 0 && b.Sort != "free" {
		switch b.Sort {
		case "popular":
			return "/" + typ + "/popular", v
		case "top":
			return "/" + typ + "/top_rated", v
		case "new":
			if typ == "movie" {
				return "/movie/now_playing", v
			}
			return "/tv/on_the_air", v
		default:
			return "/trending/" + typ + "/week", v
		}
	}
	if b.Genre > 0 {
		v.Set("with_genres", strconv.Itoa(b.Genre))
	}
	if b.Year > 0 {
		if typ == "movie" {
			v.Set("primary_release_year", strconv.Itoa(b.Year))
		} else {
			v.Set("first_air_date_year", strconv.Itoa(b.Year))
		}
	}
	switch b.Sort {
	case "free":
		// Solo títulos con disponibilidad gratuita o con anuncios en la región.
		v.Set("with_watch_monetization_types", "free,ads")
		v.Set("with_watch_region", b.Region)
		v.Set("sort_by", "popularity.desc")
	case "top":
		v.Set("sort_by", "vote_average.desc")
		v.Set("vote_count.gte", "500")
	case "new":
		if typ == "movie" {
			v.Set("sort_by", "primary_release.desc")
		} else {
			v.Set("sort_by", "first_air_date.desc")
		}
	default:
		v.Set("sort_by", "popularity.desc")
	}
	return "/discover/" + typ, v
}

func (b browse) load(ctx context.Context) (*BrowseResponse, error) {
	g, _ := genreSets(ctx) // el género es una etiqueta: si falla, seguimos sin ella

	types := []string{b.Type}
	if b.Type == "all" {
		types = []string{"movie", "tv"}
	}
	var (
		mu    sync.Mutex
		pages []tmdbPage
		errs  []error
		wg    sync.WaitGroup
	)
	for _, t := range types {
		path, params := b.endpoint(t)
		wg.Add(1)
		go func(path string, params url.Values) {
			defer wg.Done()
			var out tmdbPage
			if err := tmdbGet(ctx, path, params, &out); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			mu.Lock()
			pages = append(pages, out)
			mu.Unlock()
		}(path, params)
	}
	wg.Wait()

	resp := &BrowseResponse{Items: []Card{}, Page: b.Page, Pages: 1, Type: b.Type, Sort: b.Sort, Genre: b.Genre, Year: b.Year}
	if len(pages) == 0 {
		if len(errs) > 0 {
			return nil, errs[0]
		}
		return resp, nil
	}

	cols := make([][]Card, 0, len(pages))
	for _, pg := range pages {
		cols = append(cols, toCards(pg.Results, g, b.Type, b.Backdrops))
		if pg.TotalPages > resp.Pages {
			resp.Pages = pg.TotalPages
		}
		resp.Total += len(pg.Results)
	}
	if len(cols) == 1 {
		resp.Items = cols[0]
	} else {
		// Intercala películas y series: sin esto, "todo" siempre llenaría de cine.
		for i := 0; ; i++ {
			added := false
			for _, col := range cols {
				if i < len(col) {
					resp.Items = append(resp.Items, col[i])
					added = true
				}
			}
			if !added {
				break
			}
		}
	}
	if b.Limit > 0 && len(resp.Items) > b.Limit {
		resp.Items = resp.Items[:b.Limit]
	}
	return resp, nil
}

func parseBrowse(r *http.Request) (browse, error) {
	q := r.URL.Query()
	b := browse{
		Type:   strings.ToLower(strings.TrimSpace(q.Get("type"))),
		Sort:   strings.ToLower(strings.TrimSpace(q.Get("sort"))),
		Region: strings.ToUpper(strings.TrimSpace(q.Get("region"))),
		Limit:  parseLimit(r, browseDefLimit, browseMaxLimit),
		Page:   1,
	}
	if b.Type == "" {
		b.Type = "all"
	}
	if b.Type != "all" && b.Type != "movie" && b.Type != "tv" {
		return b, errFiltros
	}
	if b.Sort == "" {
		b.Sort = "trending"
	}
	switch b.Sort {
	case "trending", "popular", "top", "new", "free":
	default:
		return b, errFiltros
	}
	if v := q.Get("genre"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return b, errFiltros
		}
		b.Genre = n
	}
	if v := q.Get("year"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < browseMinYear || n > browseMaxYear {
			return b, errFiltros
		}
		b.Year = n
	}
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return b, errFiltros
		}
		b.Page = n
	}
	if b.Page > browseMaxPage {
		b.Page = browseMaxPage
	}
	if !countryRe.MatchString(b.Region) {
		b.Region = ""
		if cc := strings.ToUpper(r.Header.Get("X-Vercel-IP-Country")); countryRe.MatchString(cc) {
			b.Region = cc
		}
		if b.Region == "" {
			b.Region = "US" // donde TMDB tiene más datos de plataformas
		}
	}
	return b, nil
}

func browseHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	b, err := parseBrowse(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errFiltros.Error()})
		return
	}
	serveCached(w, b.key(), rowTTL, func(ctx context.Context) (any, error) {
		return b.load(ctx)
	})
}

// ---------- Destacado ----------

// pickFeatured elige un contenido destacado que rota cada ocho horas: siempre
// hay algo grande en pantalla, pero no siempre es el mismo.
func pickFeatured(cards []Card) *Card {
	pool := make([]*Card, 0, len(cards))
	for i := range cards {
		if cards[i].Backdrop != "" {
			pool = append(pool, &cards[i])
		}
	}
	if len(pool) == 0 {
		for i := range cards {
			pool = append(pool, &cards[i])
		}
	}
	slot := time.Now().Unix()/(8*3600) + int64(time.Now().Hour()/8)
	return pool[int(slot%int64(len(pool)))]
}

type FeaturedResponse struct {
	Item Card   `json:"item"`
	More []Card `json:"more"`
}

func featuredHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	serveCached(w, "featured", rowTTL, func(ctx context.Context) (any, error) {
		b := browse{Type: "all", Sort: "trending", Region: "US", Page: 1, Limit: 20, Backdrops: true}
		resp, err := b.load(ctx)
		if err != nil {
			return nil, err
		}
		if len(resp.Items) == 0 {
			return nil, errors.New("no hay nada destacado ahora mismo")
		}
		pick := pickFeatured(resp.Items)
		more := make([]Card, 0, 8)
		for _, c := range resp.Items {
			if c.ID == pick.ID && c.Type == pick.Type {
				continue
			}
			more = append(more, c)
			if len(more) == 8 {
				break
			}
		}
		return FeaturedResponse{Item: *pick, More: more}, nil
	})
}

// ---------- Similares ----------

func similarHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	typ := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if (typ != "movie" && typ != "tv") || err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parámetros type e id inválidos"})
		return
	}
	key := fmt.Sprintf("similar|%s|%d", typ, id)
	serveCached(w, key, detailTTL, func(ctx context.Context) (any, error) {
		var (
			mu  sync.Mutex
			got []tmdbItem
			wg  sync.WaitGroup
		)
		for _, suffix := range []string{"recommendations", "similar"} {
			path := fmt.Sprintf("/%s/%d/%s", typ, id, suffix)
			wg.Add(1)
			go func(path string) {
				defer wg.Done()
				var pg tmdbPage
				if err := tmdbGet(ctx, path, url.Values{"language": {"es-ES"}, "page": {"1"}}, &pg); err != nil {
					return
				}
				mu.Lock()
				got = append(got, pg.Results...)
				mu.Unlock()
			}(path)
		}
		wg.Wait()

		g, _ := genreSets(ctx)
		seen := map[int]bool{}
		items := make([]Card, 0, 20)
		for _, c := range toCards(got, g, typ, false) {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			items = append(items, c)
			if len(items) == 20 {
				break
			}
		}
		return map[string]any{"items": items}, nil
	})
}

// ---------- Fuentes para ver ----------

// Source es un sitio donde se puede buscar el título. Play marca los que se
// reproducen dentro del reproductor de la web.
type Source struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // legal | external | trailer
	Note string `json:"note,omitempty"`
	Logo string `json:"logo,omitempty"`
	URL  string `json:"url"`
	Play bool   `json:"play,omitempty"`
}

type sourceDef struct {
	name, note, host, path, param string
	kind                          string
}

var sourceDefs = []sourceDef{
	{"YouTube", "Busca la película completa", "youtube.com", "results", "search_query", "legal"},
	{"Pluto TV", "Gratis con anuncios", "pluto.tv", "on-demand/search", "query", "legal"},
	{"ViX", "Gratis con anuncios", "vix.com", "es/search", "q", "legal"},
	{"Plex", "Gratis con anuncios", "watch.plex.tv", "search", "query", "legal"},
	{"FilmAffinity", "Ficha y dónde verla", "filmaffinity.com", "es/search.php", "stext", "legal"},
	{"TMDB", "Plataformas en tu país", "themoviedb.org", "search", "query", "legal"},
	{"Cuevana", "Sitio externo", "cuevana3e.pro", "", "", "external"},
	{"Peliculon", "Sitio externo", "peliculon-tv.vercel.app", "", "", "external"},
	{"SPlayer", "Sitio externo", "splayer.lat", "", "", "external"},
}

func favicon(host string) string {
	return "https://www.google.com/s2/favicons?domain=" + url.QueryEscape(host) + "&sz=128"
}

// buildSources arma el listado de sitios donde buscar el título. Solo enlaza: la
// web no extrae ni reproduce el vídeo de terceros.
func buildSources(title, original, year, typ, trailerID string) []Source {
	name := strings.TrimSpace(title)
	if name == "" {
		name = strings.TrimSpace(original)
	}
	terms := []string{name}
	if year != "" {
		terms = append(terms, year)
	}
	if typ == "tv" {
		terms = append(terms, "serie completa")
	}
	if orig := strings.TrimSpace(original); orig != "" && !strings.EqualFold(orig, name) {
		terms = append(terms, orig)
	}
	search := strings.TrimSpace(strings.Join(terms, " "))

	out := make([]Source, 0, len(sourceDefs)+1)
	if trailerID != "" {
		out = append(out, Source{
			Name: "Tráiler", Kind: "trailer", Note: "Se reproduce aquí",
			URL:  "https://www.youtube.com/watch?v=" + url.QueryEscape(trailerID),
			Play: true,
		})
	}
	for _, d := range sourceDefs {
		u := "https://" + d.host + "/"
		if d.path != "" {
			u += d.path + "?" + url.Values{d.param: {search}}.Encode()
		}
		out = append(out, Source{
			Name: d.name, Kind: d.kind, Note: d.note, Logo: favicon(d.host), URL: u,
		})
	}
	return out
}
