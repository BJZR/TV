package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- Normalización y consulta ----------

func TestFold(t *testing.T) {
	cases := map[string]string{
		"  El Señor de los Anillos: ¡La Comunidad! ": "el senor de los anillos la comunidad",
		"Marvel's":      "marvels",
		"Spider-Man":    "spider man",
		"Amélie":        "amelie",
		"Løve":          "love",
		"Blade Runner":  "blade runner",
		"  2  Fast  2 ": "2 fast 2",
	}
	for in, want := range cases {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, quiere %q", in, got, want)
		}
	}
}

func TestParseQuery(t *testing.T) {
	p := parseQuery("batman (1989)")
	if p.Query != "batman" || p.Year != "1989" {
		t.Fatalf("año: %q %q", p.Query, p.Year)
	}
	p = parseQuery("  ver  Batman 1989 en hd ")
	if p.Query != "Batman" || p.Year != "1989" {
		t.Fatalf("ruido: %q %q", p.Query, p.Year)
	}
	p = parseQuery("2012")
	if p.Query != "2012" || p.Year != "" {
		t.Fatalf("solo año: %q %q", p.Query, p.Year)
	}
	p = parseQuery("pelicula el padrino")
	if p.Kind != "movie" || p.Query != "el padrino" {
		t.Fatalf("pista de tipo: %+v", p)
	}
	p = parseQuery("serie stranger things")
	if p.Kind != "tv" {
		t.Fatalf("serie: %+v", p)
	}
	p = parseQuery("star wars 1977-1983")
	if p.From != 1977 || p.To != 1983 || p.Fold != "star wars" {
		t.Fatalf("rango: %+v", p)
	}
	p = parseQuery("real madrid vs barcelona en vivo")
	if !p.Live {
		t.Fatalf("deportes: %+v", p)
	}
	if !parseQuery("dune").valid() || parseQuery("a").valid() {
		t.Fatalf("valid(): %+v", p)
	}
}

func TestQueryVariants(t *testing.T) {
	v := parseQuery("avengrs").variants()
	if len(v) == 0 || !strings.Contains(v[0], "aven") || v[0] == "avengrs" {
		t.Fatalf("errata sin prefijo: %q", v)
	}
	v = parseQuery("harry potter y la").variants()
	if !contains(v, "harry potter") {
		t.Fatalf("medio escribir: %q", v)
	}
	if !contains(parseQuery("the godfather").variants(), "godfather") {
		t.Fatalf("palabras vacías: %q", parseQuery("the godfather").variants())
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// ---------- Puntuación ----------

func TestTextScore(t *testing.T) {
	exact := textScore("batman", "batman")
	prefix := textScore("batman", "batman el caballero de la noche")
	other := textScore("batman", "superman")
	if !(exact > prefix && prefix > other) {
		t.Fatalf("orden inesperado: %v %v %v", exact, prefix, other)
	}
	if typo := textScore("avengrs", "avengers"); typo < 55 {
		t.Fatalf("la errata debería puntuar alto, got %v", typo)
	}
	if a, b := textScore("the matrix", "matrix"), textScore("matrix", "matrix"); a != b {
		t.Fatalf("artículos cambian el puntaje: %v vs %v", a, b)
	}
	if textScore("blade runner", "blade runner 2049") < 70 {
		t.Fatalf("títulos con número: %v", textScore("blade runner", "blade runner 2049"))
	}
	if textScore("2 fast", "2 fast 2 furious") < 70 {
		t.Fatalf("números: %v", textScore("2 fast", "2 fast 2 furious"))
	}
	if s := textScore("hanks", "tom hanks"); s < 55 {
		t.Fatalf("coincidencia por palabra suelta: %v", s)
	}
	if textScore("gato", "perro") != 0 || textScore("", "x") != 0 {
		t.Fatal("palabras sin relación deben puntuar 0")
	}
}

func TestRankPrefersYearAndQuality(t *testing.T) {
	p := parseQuery("batman 1995")
	cands := []*candidate{
		{ID: 1, Type: "movie", Title: "Batman", Date: "1989-06-23", Pop: 50, Votes: 8000, Rating: 7.2, Poster: "/a.jpg"},
		{ID: 2, Type: "movie", Title: "Batman", OrigTitle: "Batman", Date: "1995-06-16", Pop: 30, Votes: 3000, Rating: 7.9, Poster: "/b.jpg"},
		{ID: 3, Type: "movie", Title: "Batman Reloaded", Date: "2018-01-01", Pop: 90, Votes: 900, Rating: 9.9, Poster: "/c.jpg"},
	}
	res := rank(cands, p)
	if res[0].ID != 2 {
		t.Fatalf("el año manda: %+v", res)
	}
	// Un 9.9 con 900 votos no debe ganarle a un 7.9 con 3000.
	if res[0].ID == 3 {
		t.Fatalf("la votación se nota: %+v", res)
	}
}

// ---------- TMDB falso ----------

type fakeTMDB struct {
	mu       sync.Mutex
	calls    map[string]int
	broken   bool
	noPerson bool
	items    []tmdbItem
	people   map[int]struct {
		name   string
		credit []tmdbItem
	}
}

func newFake() *fakeTMDB {
	return &fakeTMDB{
		calls: map[string]int{},
		items: []tmdbItem{
			{ID: 1, MediaType: "movie", Title: "Batman", OriginalTitle: "Batman", ReleaseDate: "1989-06-23", Popularity: 50, VoteCount: 8000, VoteAverage: 7.2, PosterPath: "/a.jpg", Overview: "Un caballero oscuro"},
			{ID: 2, MediaType: "movie", Title: "Batman Forever", OriginalTitle: "Batman Forever", ReleaseDate: "1995-06-16", Popularity: 30, VoteCount: 3000, VoteAverage: 7.9, PosterPath: "/b.jpg", Overview: "x"},
			{ID: 3, MediaType: "movie", Title: "Batman Reloaded", ReleaseDate: "2018-01-01", Popularity: 90, VoteCount: 900, VoteAverage: 9.9, PosterPath: "/c.jpg"},
			{ID: 10, MediaType: "movie", Title: "Avengers", OriginalTitle: "Avengers", ReleaseDate: "2012-04-25", Popularity: 80, VoteCount: 24000, VoteAverage: 7.7, PosterPath: "/d.jpg", Overview: "x"},
			{ID: 11, MediaType: "movie", Title: "Avenue 5", ReleaseDate: "2020-01-01", Popularity: 5, VoteCount: 40, VoteAverage: 5.9, PosterPath: "/e.jpg"},
			{ID: 12, MediaType: "movie", Title: "Avenge", ReleaseDate: "2015-01-01", Popularity: 3, VoteCount: 20, VoteAverage: 4.1},
		},
		people: map[int]struct {
			name   string
			credit []tmdbItem
		}{
			7: {"Tom Hanks", []tmdbItem{
				{ID: 20, MediaType: "movie", Title: "Forrest Gump", ReleaseDate: "1994-07-06", Popularity: 60, VoteCount: 21000, VoteAverage: 8.5, PosterPath: "/f.jpg"},
				{ID: 21, MediaType: "movie", Title: "Un papel pequeño", ReleaseDate: "2000-01-01", Popularity: 2, VoteCount: 10, VoteAverage: 5.0},
			}},
		},
	}
}

func (f *fakeTMDB) matches(q string) bool {
	q = fold(q)
	if q == "" {
		return false
	}
	for _, it := range f.items {
		t := fold(it.title())
		if t == q || strings.HasPrefix(t, q) || strings.Contains(t, q) {
			return true
		}
	}
	for _, p := range f.people {
		if strings.Contains(fold(p.name), q) || strings.Contains(q, fold(p.name)) {
			return true
		}
	}
	return false
}

func (f *fakeTMDB) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[r.URL.Path+"?"+r.URL.Query().Get("query")]++

		if f.broken {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")

		switch {
		case r.URL.Path == "/search/multi":
			lang := r.URL.Query().Get("language")
			results := []map[string]any{}
			if f.matches(q) {
				for _, it := range f.items {
					t := fold(it.title())
					if lang == "es-ES" && it.Title != "Avengers" {
						continue // el catálogo "en español" casi no tiene nada
					}
					if t == fold(q) || strings.HasPrefix(t, fold(q)) || strings.Contains(t, fold(q)) {
						results = append(results, map[string]any{
							"id": it.ID, "media_type": it.MediaType, "title": it.Title,
							"original_title": it.OriginalTitle, "release_date": it.ReleaseDate,
							"popularity": it.Popularity, "vote_count": it.VoteCount,
							"vote_average": it.VoteAverage, "poster_path": it.PosterPath,
							"overview": it.Overview,
						})
					}
				}
				for id, p := range f.people {
					if strings.Contains(fold(p.name), fold(q)) || strings.HasPrefix(fold(p.name), fold(q)) {
						known := []map[string]any{}
						for _, c := range p.credit {
							known = append(known, map[string]any{
								"id": c.ID, "media_type": "movie", "title": c.Title,
								"release_date": c.ReleaseDate, "popularity": c.Popularity,
								"vote_count": c.VoteCount, "vote_average": c.VoteAverage,
								"poster_path": c.PosterPath,
							})
						}
						results = append(results, map[string]any{
							"id": id, "media_type": "person", "name": p.name, "known_for": known,
						})
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"results": results})

		case strings.HasPrefix(r.URL.Path, "/person/"):
			if f.noPerson {
				http.NotFound(w, r)
				return
			}
			var id int
			json.Unmarshal([]byte(strings.Trim(r.URL.Path, "/person/")), &id)
			p, ok := f.people[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			movies := []map[string]any{}
			for _, c := range p.credit {
				movies = append(movies, map[string]any{
					"id": c.ID, "media_type": "movie", "title": c.Title,
					"release_date": c.ReleaseDate, "popularity": c.Popularity,
					"vote_count": c.VoteCount, "vote_average": c.VoteAverage,
					"poster_path": c.PosterPath,
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "name": p.name,
				"movie_credits": map[string]any{"cast": movies}})

		case strings.HasPrefix(r.URL.Path, "/movie/"), strings.HasPrefix(r.URL.Path, "/tv/"):
			var id int
			fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &id)
			title, date, runtime, seasons := "Batman", "1989-06-23", 126, 0
			for _, it := range f.items {
				if id == it.ID {
					title, date = it.Title, it.ReleaseDate
				}
			}
			body := map[string]any{
				"id": id, "title": title, "release_date": date, "runtime": runtime,
				"genres":            []map[string]any{{"name": "Fantasía"}},
				"videos":            map[string]any{"results": []map[string]any{{"key": "t1", "site": "YouTube", "type": "Teaser"}, {"key": "t2", "site": "YouTube", "type": "Trailer", "official": true}}},
				"watch/providers":   map[string]any{"results": map[string]any{"CO": map[string]any{"link": "https://tmdb/watch", "flatrate": []map[string]any{{"provider_name": "Max", "logo_path": "/m.jpg"}, {"provider_name": "Max", "logo_path": "/m.jpg"}}}}},
				"original_title":    "Batman",
				"first_air_date":    date,
				"number_of_seasons": seasons,
			}
			json.NewEncoder(w).Encode(body)

		default:
			http.NotFound(w, r)
		}
	})
}

func startFake(t *testing.T) *fakeTMDB {
	t.Helper()
	f := newFake()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	tmdbBase = srv.URL
	os.Setenv("TMDB_API_KEY", "test")
	cacheFlush()
	t.Cleanup(func() {
		os.Unsetenv("TMDB_API_KEY")
		cacheFlush()
	})
	return f
}

// ---------- Búsqueda ----------

func TestSearchDirectAndYear(t *testing.T) {
	startFake(t)
	resp, err := runSearch(context.Background(), "batman", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 || resp.Results[0].ID != 1 {
		t.Fatalf("ranking: %+v", resp.Results)
	}
	if resp.Results[0].Title != "Batman" || resp.Results[0].Year != "1989" {
		t.Fatalf("datos: %+v", resp.Results[0])
	}

	resp, err = runSearch(context.Background(), "batman 1995", 10, false)
	if err != nil || resp.Year != "1995" || resp.Results[0].ID != 2 {
		t.Fatalf("el año debería priorizar Batman Forever: %+v %v", resp.Results, err)
	}
}

func TestSearchTypoRecovers(t *testing.T) {
	f := startFake(t)
	resp, err := runSearch(context.Background(), "avengrs", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 || resp.Results[0].ID != 10 {
		t.Fatalf("la errata debería encontrar Avengers: %+v", resp.Results)
	}
	if resp.DidYouMean != "Avengers" {
		t.Fatalf("didYouMean: %+v", resp)
	}
	if len(f.calls) == 0 {
		t.Fatal("no se consultó TMDB")
	}
	// La primera consulta va literal; el prefijo es el plan B.
	if f.calls["/search/multi?avengrs"] == 0 {
		t.Fatalf("no buscó la errata tal cual: %v", f.calls)
	}
	if f.calls["/search/multi?aven"] == 0 {
		t.Fatalf("no buscó por prefijo: %v", f.calls)
	}
}

func TestSearchPerson(t *testing.T) {
	f := startFake(t)
	resp, err := runSearch(context.Background(), "tom hanks", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 || resp.Results[0].Via != "Tom Hanks" {
		t.Fatalf("known_for: %+v", resp.Results)
	}
	if resp.Results[0].ID != 20 {
		t.Fatalf("trae la filmografía: %+v", resp.Results)
	}
	if f.calls["/person/7?"] == 0 {
		t.Fatalf("no pidió la filmografía: %v", f.calls)
	}
	for _, r := range resp.Results {
		if r.ID == 21 {
			t.Fatalf("papeles sin votos no deberían salir: %+v", r)
		}
	}
}

func TestSearchPersonCreditsFailure(t *testing.T) {
	f := startFake(t)
	f.noPerson = true
	resp, err := runSearch(context.Background(), "tom hanks", 10, false)
	if err != nil || len(resp.Results) == 0 || resp.Results[0].Via != "Tom Hanks" {
		t.Fatalf("debe seguir funcionando sin la filmografía: %v %+v", err, resp)
	}
}

func TestSearchSports(t *testing.T) {
	startFake(t)
	resp, err := runSearch(context.Background(), "real madrid vs barcelona", 10, false)
	if err != nil || len(resp.Results) != 1 || resp.Results[0].Type != "live" {
		t.Fatalf("deportes: %v %+v", err, resp)
	}
	if !strings.Contains(resp.Results[0].URL, "youtube.com") {
		t.Fatalf("url en vivo: %+v", resp.Results[0])
	}
}

func TestSearchEmptyAndLimit(t *testing.T) {
	startFake(t)
	resp, err := runSearch(context.Background(), "  ", 10, false)
	if err != nil || len(resp.Results) != 0 || resp.Total != 0 {
		t.Fatalf("vacío: %v %+v", err, resp)
	}
	resp, err = runSearch(context.Background(), "batman", 1, false)
	if err != nil || resp.Total < 2 || len(resp.Results) != 1 {
		t.Fatalf("límite: %+v", err)
	}
}

func TestQuickSearchSkipsRecovery(t *testing.T) {
	f := startFake(t)
	if _, err := runSearch(context.Background(), "avengrs", 6, true); err != nil {
		t.Fatal(err)
	}
	if f.calls["/search/multi?aven"] != 0 {
		t.Fatalf("las sugerencias no deben gastar el plan B: %v", f.calls)
	}
}

// ---------- Caché ----------

func TestCacheServesAndExpires(t *testing.T) {
	f := startFake(t)
	if _, err := runSearch(context.Background(), "batman", 10, false); err != nil {
		t.Fatal(err)
	}
	first := f.calls["/search/multi?batman"]
	if _, err := runSearch(context.Background(), "batman", 10, false); err != nil {
		t.Fatal(err)
	}
	if f.calls["/search/multi?batman"] != first {
		t.Fatalf("no reutilizó la caché: %d vs %d", f.calls["/search/multi?batman"], first)
	}

	cacheMu.Lock()
	for _, el := range cacheMap {
		el.Value.(*cacheItem).at = time.Now().Add(-time.Hour)
	}
	cacheMu.Unlock()
	if _, err := runSearch(context.Background(), "batman", 10, false); err != nil {
		t.Fatal(err)
	}
	if f.calls["/search/multi?batman"] <= first {
		t.Fatal("un caché vencido debería volver a preguntar a TMDB")
	}
}

func TestCacheEviction(t *testing.T) {
	cacheFlush()
	for i := 0; i < cacheMax+50; i++ {
		cacheSet(cacheKeyN(i), []*candidate{{ID: i}}, cacheTTL, false)
	}
	cacheMu.Lock()
	n := len(cacheMap)
	cacheMu.Unlock()
	if n > cacheMax {
		t.Fatalf("caché sin límite: %d", n)
	}
}

func TestCacheStaleFallback(t *testing.T) {
	f := startFake(t)
	if _, err := runSearch(context.Background(), "batman", 10, false); err != nil {
		t.Fatal(err)
	}
	cacheMu.Lock()
	for _, el := range cacheMap {
		it := el.Value.(*cacheItem)
		it.at = time.Now().Add(-2 * time.Hour)
		it.ttl = time.Minute
	}
	cacheMu.Unlock()

	f.broken = true
	resp, err := runSearch(context.Background(), "batman", 10, false)
	if err != nil {
		t.Fatalf("con TMDB caído debe servir lo viejo: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("sin resultados")
	}
}

func TestCacheNegative(t *testing.T) {
	f := startFake(t)
	if _, err := runSearch(context.Background(), "noexisteeste123", 10, false); err != nil {
		t.Fatal(err)
	}
	before := f.calls["/search/multi?noexisteeste123"]
	if _, err := runSearch(context.Background(), "noexisteeste123", 10, false); err != nil {
		t.Fatal(err)
	}
	if f.calls["/search/multi?noexisteeste123"] != before {
		t.Fatal("el vacío también se debe cachear para no castigar a TMDB")
	}
}

func cacheKeyN(i int) string { return "k" + strconv.Itoa(i) }

// ---------- Handlers ----------

func TestHandlers(t *testing.T) {
	startFake(t)

	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=batman&limit=1", nil))
	var sr SearchResponse
	json.Unmarshal(rec.Body.Bytes(), &sr)
	if rec.Code != 200 || len(sr.Results) != 1 || sr.Total < 2 {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Fatal("sin Cache-Control en la búsqueda")
	}

	rec = httptest.NewRecorder()
	suggestHandler(rec, httptest.NewRequest("GET", "/api/suggest?q=batman", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "results") {
		t.Fatalf("suggest: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	titleHandler(rec, httptest.NewRequest("GET", "/api/title?type=movie&id=1&country=CO", nil))
	var d Details
	json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != 200 || d.Trailer != "https://www.youtube.com/watch?v=t2" {
		t.Fatalf("title: %d %s", rec.Code, rec.Body.String())
	}
	if len(d.Providers) != 1 {
		t.Fatalf("proveedores duplicados: %+v", d.Providers)
	}

	for _, bad := range []string{
		"/api/title?type=person&id=1",
		"/api/title?type=movie&id=0",
		"/api/title?type=movie&id=abc",
	} {
		rec = httptest.NewRecorder()
		titleHandler(rec, httptest.NewRequest("GET", bad, nil))
		if rec.Code != 400 {
			t.Fatalf("%s debería dar 400, dio %d", bad, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("OPTIONS", "/api/search?q=batman", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS: %d", rec.Code)
	}
}

// TestCompressGzipYDocsPages evita el fallo más caro del proyecto: comprimir la
// respuesta sin mandar Content-Encoding hacía que el navegador viera binario gzip
// en lugar del HTML.
func TestCompressGzipYDocsPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/catalogo", pageHandler("/catalogo", "catalog.html"))
	mux.HandleFunc("/", homeHandler)
	h := compress(mux)

	for _, path := range []string{"/", "/catalogo"} {
		// Sin gzip: HTML plano.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Fatalf("%s sin gzip: %d %q", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("%s Content-Type: %q", path, ct)
		}

		// Con gzip: cabecera presente y cuerpo que descomprime al mismo HTML.
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("%s Content-Encoding: %q", path, got)
		}
		if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
			t.Fatalf("%s sin Vary: Accept-Encoding", path)
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("%s gzip ilegible: %v", path, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%s no se descomprime: %v", path, err)
		}
		if !strings.Contains(string(plain), "<!DOCTYPE html>") {
			t.Fatalf("%s HTML comprimido ilegible: %q", path, string(plain[:60]))
		}
	}

	// HEAD no debe comprimirse.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("HEAD", "/", nil))
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("HEAD comprimido: %q", got)
	}
}

// TestPagesEnlazadas verifica que el launcher y el catálogo se alcanzan entre sí.
func TestPagesEnlazadas(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/catalogo", pageHandler("/catalogo", "catalog.html"))
	mux.HandleFunc("/catalogo/", pageHandler("/catalogo", "catalog.html"))
	mux.HandleFunc("/", homeHandler)

	// Con barra final y sin ella: las dos formas del enlace deben servir la página.
	for _, path := range []string{"/", "/catalogo", "/catalogo/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s debería servir la página, dio %d", path, rec.Code)
		}
	}

	// Enlaces cruzados: cada página apunta a la otra, con respaldo al archivo
	// .html por si se abre sin el servidor de Go.
	home, err := site.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := site.ReadFile("catalog.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(home), `href="/catalogo"`) || !strings.Contains(string(home), `link.href = 'catalog.html'`) {
		t.Fatal("index.html no enlaza al catálogo (ni cae al archivo)")
	}
	if !strings.Contains(string(cat), `href="/"`) || !strings.Contains(string(cat), `link.href = 'index.html'`) {
		t.Fatal("catalog.html no vuelve al launcher (ni cae al archivo)")
	}
}

func TestNoAPIKey(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	tmdbBase = srv.URL
	os.Setenv("TMDB_API_KEY", "test")
	cacheFlush()
	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=batman", nil))
	if rec.Code != 200 {
		t.Fatalf("con clave: %d", rec.Code)
	}

	os.Unsetenv("TMDB_API_KEY")
	cacheFlush()
	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=batman", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "TMDB_API_KEY") {
		t.Fatalf("sin clave: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRateLimit(t *testing.T) {
	f := newFake()
	f.broken = true
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	tmdbBase = srv.URL
	os.Setenv("TMDB_API_KEY", "test")
	cacheFlush()
	defer cacheFlush()
	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=loquesea", nil))
	if rec.Code != 502 && rec.Code != 503 {
		t.Fatalf("TMDB caído: %d %s", rec.Code, rec.Body.String())
	}
}
