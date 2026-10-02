package main

import (
	"container/list"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubTMDB levanta un TMDB falso: guarda las URLs pedidas y devuelve las
// respuestas que el test le indica.
func stubTMDB(t *testing.T, routes map[string]any) *[]string {
	t.Helper()
	seen := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.RequestURI())
		body, ok := routes[strings.Split(r.URL.Path, "?")[0]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status_message":"not found"}`))
			return
		}
		json.NewEncoder(w).Encode(body)
	}))
	old := tmdbBase
	tmdbBase = srv.URL
	t.Setenv("TMDB_API_KEY", "test")
	t.Cleanup(func() {
		tmdbBase = old
		os.Unsetenv("TMDB_API_KEY")
		cacheFlush()
	})
	return seen
}

func item(id int, title, date string, genre int, rating float64, votes int) map[string]any {
	return map[string]any{
		"id": id, "title": title, "overview": "sinopsis de " + title,
		"release_date": date, "vote_average": rating, "vote_count": votes,
		"poster_path": "/p.jpg", "backdrop_path": "/b.jpg", "popularity": 12.5,
		"genre_ids": []int{genre},
	}
}

func tvItem(id int, name, date string, genre int) map[string]any {
	return map[string]any{
		"id": id, "name": name, "overview": "sinopsis de " + name,
		"first_air_date": date, "vote_average": 8.4, "vote_count": 900,
		"poster_path": "/p.jpg", "backdrop_path": "/b.jpg", "popularity": 30,
		"genre_ids": []int{genre},
	}
}

func page(items ...any) map[string]any {
	return map[string]any{"page": 1, "total_pages": 4, "results": items}
}

func genresPayload() any {
	return map[string]any{"genres": []any{
		map[string]any{"id": 28, "name": "Acción"},
		map[string]any{"id": 18, "name": "Drama"},
	}}
}

// ---------- Filtros ----------

func TestParseBrowse(t *testing.T) {
	ok := []string{
		"", "type=all", "type=movie&sort=top", "sort=free&type=tv",
		"genre=28", "genre=28&year=1999", "page=3&limit=48",
	}
	for _, q := range ok {
		req := httptest.NewRequest("GET", "/api/browse?"+q, nil)
		if _, err := parseBrowse(req); err != nil {
			t.Errorf("parseBrowse(%q) = %v, quiere nil", q, err)
		}
	}
	bad := []string{"type=person", "sort=inventado", "genre=abc", "genre=-1", "year=99", "page=0"}
	for _, q := range bad {
		req := httptest.NewRequest("GET", "/api/browse?"+q, nil)
		if _, err := parseBrowse(req); err == nil {
			t.Errorf("parseBrowse(%q) = nil, quiere error", q)
		}
	}

	// La región siempre queda resuelta: es parte de la clave de caché.
	req := httptest.NewRequest("GET", "/api/browse", nil)
	b, _ := parseBrowse(req)
	if b.Region != "US" || b.Limit != browseDefLimit || b.Page != 1 || b.Sort != "trending" {
		t.Fatalf("valores por defecto: %+v", b)
	}
	req = httptest.NewRequest("GET", "/api/browse?region=mx&page=999", nil)
	b, _ = parseBrowse(req)
	if b.Region != "MX" || b.Page != browseMaxPage {
		t.Fatalf("región y tope de página: %+v", b)
	}
}

func TestBrowseEndpoint(t *testing.T) {
	cases := []struct {
		b    browse
		path string
		key  string
		val  string
	}{
		{browse{Type: "all", Sort: "trending", Page: 1}, "/trending/all/week", "", ""},
		{browse{Type: "movie", Sort: "popular", Page: 1}, "/movie/popular", "", ""},
		{browse{Type: "tv", Sort: "popular", Page: 1}, "/tv/popular", "", ""},
		{browse{Type: "movie", Sort: "top", Page: 1}, "/movie/top_rated", "", ""},
		{browse{Type: "movie", Sort: "new", Page: 1}, "/movie/now_playing", "", ""},
		{browse{Type: "tv", Sort: "new", Page: 1}, "/tv/on_the_air", "", ""},
		{browse{Type: "movie", Sort: "free", Region: "CO", Page: 1}, "/discover/movie", "with_watch_monetization_types", "free,ads"},
		{browse{Type: "movie", Genre: 28, Page: 1}, "/discover/movie", "with_genres", "28"},
		{browse{Type: "movie", Genre: 28, Year: 1999, Page: 1}, "/discover/movie", "primary_release_year", "1999"},
		{browse{Type: "tv", Year: 1999, Page: 1}, "/discover/tv", "first_air_date_year", "1999"},
	}
	for _, c := range cases {
		path, v := c.b.endpoint(c.b.Type)
		if path != c.path {
			t.Errorf("%+v: path = %q, quiere %q", c.b, path, c.path)
		}
		if c.key != "" && v.Get(c.key) != c.val {
			t.Errorf("%+v: %s = %q, quiere %q", c.b, c.key, v.Get(c.key), c.val)
		}
		if v.Get("language") != "es-ES" || v.Get("include_adult") != "false" {
			t.Errorf("%+v: faltan los parámetros base: %v", c.b, v)
		}
	}

	// Un filtro de año o género obliga a /discover aunque no se pida discovery.
	if _, v := (browse{Type: "movie", Sort: "top", Year: 2001, Page: 1}).endpoint("movie"); v.Get("primary_release_year") != "2001" {
		t.Error("año con sort=top debe ir a /discover")
	}
}

// ---------- Carga ----------

func TestBrowseAllIntercala(t *testing.T) {
	seen := stubTMDB(t, map[string]any{
		"/trending/movie/week": page(
			item(1, "A", "2020-01-01", 28, 8, 900),
			item(2, "B", "2021-01-01", 18, 7, 900),
			item(3, "C", "2022-01-01", 28, 6, 900),
		),
		"/trending/tv/week": page(
			tvItem(11, "S1", "2019-01-01", 18),
			tvItem(12, "S2", "2020-01-01", 28),
			tvItem(13, "S3", "2021-01-01", 18),
		),
	})

	b := browse{Type: "all", Sort: "trending", Region: "US", Page: 1, Limit: 10}
	resp, err := b.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 6 {
		t.Fatalf("items = %d, quiere 6", len(resp.Items))
	}
	// Alterna película, serie, película… para que "todo" no se llene de cine.
	for i := 0; i < len(resp.Items); i += 2 {
		if i+1 >= len(resp.Items) {
			break
		}
		if resp.Items[i].Type == resp.Items[i+1].Type {
			t.Fatalf("pos %d y %d son %s: no intercaló", i, i+1, resp.Items[i].Type)
		}
	}
	trending := 0
	for _, u := range *seen {
		if strings.HasPrefix(u, "/trending/") {
			trending++
		}
	}
	if trending != 2 {
		t.Errorf("peticiones de tendencia = %d, quiere 2 (una por tipo)", trending)
	}
	if resp.Pages != 4 {
		t.Errorf("pages = %d, quiere 4", resp.Pages)
	}
}

func TestBrowseRespetaLimite(t *testing.T) {
	stubTMDB(t, map[string]any{
		"/movie/popular": page(
			item(1, "A", "2020-01-01", 28, 8, 900),
			item(2, "B", "2021-01-01", 18, 7, 900),
		),
	})
	b := browse{Type: "movie", Sort: "popular", Region: "US", Page: 1, Limit: 1}
	resp, err := b.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Total != 2 {
		t.Fatalf("items=%d total=%d, quiere 1 y 2", len(resp.Items), resp.Total)
	}
}

func TestToCardsFiltraYEtiqueta(t *testing.T) {
	stubTMDB(t, map[string]any{
		"/genre/movie/list": genresPayload(),
		"/genre/tv/list":    genresPayload(),
	})
	g, err := genreSets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := []tmdbItem{
		{ID: 1, MediaType: "movie", Title: "Con póster", ReleaseDate: "2020-05-01", VoteAverage: 7.6, VoteCount: 900, GenreIDs: []int{28}, PosterPath: "/p.jpg", BackdropPath: "/b.jpg"},
		{ID: 2, MediaType: "person", Name: "Alguien"},
		{ID: 3, MediaType: "movie", Title: "Sin votes", ReleaseDate: "2021-05-01", VoteAverage: 9.9, VoteCount: 3, GenreIDs: []int{18}},
		{ID: 4, MediaType: "movie", Title: "", ReleaseDate: "2021-05-01"},
	}
	cards := toCards(items, g, "", true)
	if len(cards) != 2 {
		t.Fatalf("cards = %d, quiere 2 (personas y vacíos se descartan)", len(cards))
	}
	if cards[0].Genre != "Acción" {
		t.Errorf("género = %q, quiere Acción", cards[0].Genre)
	}
	if cards[0].Backdrop == "" || cards[0].Poster == "" {
		t.Error("con backdrops=true deben venir póster y fondo")
	}
	if cards[1].Rating != 0 {
		t.Errorf("nota con 3 votos = %v, quiere 0", cards[1].Rating)
	}

	// SinGenres no rompe nada: la etiqueta simplemente no aparece.
	plain := toCards(items[:1], nil, "", false)
	if len(plain) != 1 || plain[0].Genre != "" || plain[0].Backdrop != "" {
		t.Errorf("sin géneros ni fondos: %+v", plain[0])
	}

	// El tipo se deduce del campo que venga cuando no hay media_type.
	deducido := toCards([]tmdbItem{{ID: 5, Name: "Serie", FirstAirDate: "2020-01-01"}, {ID: 6, Title: "Película", ReleaseDate: "2020-01-01"}}, nil, "", false)
	if deducido[0].Type != "tv" || deducido[1].Type != "movie" {
		t.Errorf("tipos deducidos: %q %q", deducido[0].Type, deducido[1].Type)
	}
}

// ---------- Fuentes ----------

func TestBuildSources(t *testing.T) {
	s := buildSources("Dune: Parte Dos", "Dune: Part Two", "2024", "movie", "abc123")
	if len(s) != len(sourceDefs)+1 {
		t.Fatalf("sources = %d, quiere %d", len(s), len(sourceDefs)+1)
	}
	if !s[0].Play || s[0].Kind != "trailer" || !strings.Contains(s[0].URL, "v=abc123") {
		t.Errorf("el tráiler debe ir primero y ser reproducible: %+v", s[0])
	}
	var yt, cuevana *Source
	for i := range s {
		switch s[i].Name {
		case "YouTube":
			yt = &s[i]
		case "Cuevana":
			cuevana = &s[i]
		}
	}
	if yt == nil || !strings.Contains(yt.URL, "Dune") || !strings.Contains(yt.URL, "2024") || !strings.Contains(yt.URL, "Dune%3A+Part+Two") {
		t.Errorf("búsqueda en YouTube mal formada: %+v", yt)
	}
	if cuevana == nil || cuevana.Kind != "external" || cuevana.Play {
		t.Errorf("los sitios externos solo se enlazan: %+v", cuevana)
	}
	// Sin tráiler no aparece esa fila, y nadie reproduce vídeo de terceros.
	s = buildSources("Silo", "", "2023", "tv", "")
	if len(s) != len(sourceDefs) {
		t.Errorf("sin tráiler: sources = %d, quiere %d", len(s), len(sourceDefs))
	}
	for _, x := range s {
		if x.Play {
			t.Errorf("%s no debería reproducirse aquí", x.Name)
		}
		if !strings.HasPrefix(x.URL, "https://") {
			t.Errorf("%s con URL insegura: %s", x.Name, x.URL)
		}
	}
	if !strings.Contains(s[0].URL, "serie+completa") {
		t.Errorf("una serie debe buscar la temporada completa: %s", s[0].URL)
	}
}

// ---------- Caché ----------

func TestResponseCache(t *testing.T) {
	rcMu.Lock()
	rcMap = map[string]*list.Element{}
	rcLRU = list.New()
	rcMu.Unlock()

	rcSet("k", []byte(`{"a":1}`))
	if b, ok := rcGet("k", time.Minute); !ok || string(b) != `{"a":1}` {
		t.Fatal("recién guardado tiene que estar disponible")
	}
	if _, ok := rcGet("k", 0); ok {
		t.Error("un TTL vencido no debe servir fresco")
	}
	if _, ok := rcStale("k"); !ok {
		t.Error("vencido pero dentro de la ventana de seguridad: sí sirve")
	}
	if _, ok := rcStale("noexiste"); ok {
		t.Error("una clave desconocida no existe")
	}

	// El tope de entradas no puede dejar el mapa y la lista desalineados.
	for i := 0; i < rcMax+50; i++ {
		rcSet("k"+strconv.Itoa(i), []byte("{}"))
	}
	rcMu.Lock()
	n, l := len(rcMap), rcLRU.Len()
	rcMu.Unlock()
	if n != l || n > rcMax {
		t.Fatalf("mapa=%d lista=%d tope=%d: se desincronizaron", n, l, rcMax)
	}

	rcSet("k", []byte(`{"b":2}`))
	if b, _ := rcGet("k", time.Minute); string(b) != `{"b":2}` {
		t.Errorf("reescribir una clave debe actualizar el cuerpo: %s", b)
	}
}

func TestFetchOnceDedup(t *testing.T) {
	anyMu.Lock()
	anyIn = map[string]*anyCall{}
	anyMu.Unlock()

	var calls int
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := fetchOnce(context.Background(), "dedup", func(ctx context.Context) (any, error) {
				calls++
				time.Sleep(60 * time.Millisecond)
				return "uno", nil
			})
			if err != nil || v != "uno" {
				t.Errorf("fetchOnce = %v, %v", v, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Errorf("la función corrió %d veces, quiere 1", calls)
	}
}

// ---------- Manejadores ----------

func TestBrowseHandler(t *testing.T) {
	seen := stubTMDB(t, map[string]any{
		"/trending/movie/week": page(item(1, "A", "2020-01-01", 28, 8, 900)),
		"/genre/movie/list":    genresPayload(),
		"/genre/tv/list":       genresPayload(),
	})

	w := httptest.NewRecorder()
	browseHandler(w, httptest.NewRequest("GET", "/api/browse?type=movie&limit=5", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, quiere 200 (%s)", w.Code, w.Body.String())
	}
	var got BrowseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Title != "A" {
		t.Fatalf("items = %+v", got.Items)
	}
	if w.Header().Get("X-Cache") != "MISS" {
		t.Errorf("primera llamada X-Cache = %q, quiere MISS", w.Header().Get("X-Cache"))
	}

	// La segunda debe salir de la caché sin volver a pegarle a TMDB.
	before := len(*seen)
	w = httptest.NewRecorder()
	browseHandler(w, httptest.NewRequest("GET", "/api/browse?type=movie&limit=5", nil))
	if w.Code != http.StatusOK || w.Header().Get("X-Cache") != "HIT" {
		t.Errorf("segunda llamada: code=%d X-Cache=%q, quiere 200/HIT", w.Code, w.Header().Get("X-Cache"))
	}
	if len(*seen) != before {
		t.Errorf("la caché golpeó TMDB: %d -> %d", before, len(*seen))
	}

	w = httptest.NewRecorder()
	browseHandler(w, httptest.NewRequest("GET", "/api/browse?sort=inventado", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("filtro inválido: code = %d, quiere 400", w.Code)
	}
}

func TestGenresHandler(t *testing.T) {
	stubTMDB(t, map[string]any{
		"/genre/movie/list": genresPayload(),
		"/genre/tv/list":    genresPayload(),
	})
	w := httptest.NewRecorder()
	genresHandler(w, httptest.NewRequest("GET", "/api/genres", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, quiere 200", w.Code)
	}
	var got GenresResponse
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Movie) != 2 || len(got.TV) != 2 || got.Movie[0].Name != "Acción" {
		t.Errorf("géneros: %+v", got)
	}
}

func TestSimilarHandler(t *testing.T) {
	seen := stubTMDB(t, map[string]any{
		"/movie/550/recommendations": page(item(1, "A", "2020-01-01", 28, 8, 900)),
		"/movie/550/similar":         page(item(1, "A", "2020-01-01", 28, 8, 900), item(2, "B", "2021-01-01", 28, 7, 800)),
		"/genre/movie/list":          genresPayload(),
		"/genre/tv/list":             genresPayload(),
	})
	w := httptest.NewRecorder()
	similarHandler(w, httptest.NewRequest("GET", "/api/similar?type=movie&id=550", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Items []Card `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	// recommendations y similar traen los mismos: deben quedar sin repetir.
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, quiere 2 sin duplicados", len(got.Items))
	}
	for _, it := range got.Items {
		if it.Type != "movie" {
			t.Errorf("%q salió como %q: el tipo se hereda del endpoint", it.Title, it.Type)
		}
	}
	if len(*seen) < 2 {
		t.Errorf("peticiones = %d, quiere al menos 2", len(*seen))
	}

	for _, q := range []string{"?type=movie&id=0", "?type=person&id=1", "?type=movie&id=x", ""} {
		w := httptest.NewRecorder()
		similarHandler(w, httptest.NewRequest("GET", "/api/similar"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("similar%s = %d, quiere 400", q, w.Code)
		}
	}
}
