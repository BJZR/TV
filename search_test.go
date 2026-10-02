package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- Utilidades de texto ----------

func TestFold(t *testing.T) {
	for in, want := range map[string]string{
		"El Señor de los Anillos": "el senor de los anillos",
		"Amélie":                  "amelie",
		"  doble   espacio ":      "doble espacio",
		"Spider-Man: No Way Home": "spider man no way home",
	} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, quiere %q", in, got, want)
		}
	}
}

func TestTextScore(t *testing.T) {
	if textScore("matrix", "matrix") < 99 {
		t.Error("un título igual debería puntuar casi 100")
	}
	if textScore("matrix", "the matrix") < 80 {
		t.Error("con artículo delante también es el mismo título")
	}
	if textScore("matrix", "batman begins") > 40 {
		t.Error("títulos distintos no deberían parecerse")
	}
	if textScore("harry potter", "harry potter y la piedra filosofal") < 70 {
		t.Error("el título completo sigue siendo la película")
	}
}

func TestSimilarityToleratesTypos(t *testing.T) {
	if similarity("avengrs", "avengers") < 0.6 {
		t.Error("una errata no debería hundir la similitud")
	}
	if similarity("matrix", "batman begins") > 0.5 {
		t.Error("palabras sin relación son distintas")
	}
}

// ---------- Consulta ----------

func TestParseQuery(t *testing.T) {
	p := parseQuery("  ver matrix 1999 en hd  ")
	if p.Query != "matrix" {
		t.Errorf("query limpia = %q", p.Query)
	}
	if p.Year != "1999" {
		t.Errorf("año = %q", p.Year)
	}

	if got := parseQuery("pelicula blade runner").Kind; got != "movie" {
		t.Errorf("pista de tipo = %q", got)
	}
	if got := parseQuery("serie dark").Kind; got != "tv" {
		t.Errorf("pista de serie = %q", got)
	}
	if p := parseQuery("star wars 1977-1983"); p.From != 1977 || p.To != 1983 {
		t.Errorf("rango = %d-%d", p.From, p.To)
	}
	if !parseQuery("chelsea barcelona partido").Live {
		t.Error("debería detectar intención deportiva")
	}
	if parseQuery("").valid() {
		t.Error("una consulta vacía no es válida")
	}
	if !parseQuery("serie m").valid() {
		t.Error("una letra con pista de tipo sí es válida")
	}
	if parseQuery(strings.Repeat("a", 300)).Query == "" {
		t.Error("una consulta enorme debe recortarse, no romperse")
	}
}

func TestWebQueries(t *testing.T) {
	got := parseQuery("matrix 1999").webQueries()
	if len(got) < 2 || got[0] != "matrix" {
		t.Fatalf("consultas = %v", got)
	}
	found := false
	for _, q := range got {
		if q == "matrix ver en línea" {
			found = true
		}
	}
	if !found {
		t.Errorf("falta la variante para ver en línea: %v", got)
	}

	// Sin duplicados.
	seen := map[string]bool{}
	for _, q := range got {
		if seen[q] {
			t.Errorf("consulta repetida %q", q)
		}
		seen[q] = true
	}
	if parseQuery("matrix").Live {
		t.Error("sin palabras deportivas no debe marcar Live")
	}
}

// ---------- Lectura del HTML del buscador ----------

const fixtureHTML = `
<div class="result results_links web-result">
  <h2 class="result__title"><a rel="nofollow" class="result__a"
     href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fwww.netflix.com%2Fes%2Ftitle%2F80100172&amp;rut=abc">Matrix | Netflix</a></h2>
  <a class="result__snippet" href="x">Matrix (1999) —película de ciencia ficción disponible en <b>streaming</b>.</a>
</div>
<div class="result results_links web-result">
  <h2 class="result__title"><a rel="nofollow" class="result__a"
     href="https://es.wikipedia.org/wiki/Matrix">Matrix - Wikipedia, la enciclopedia libre</a></h2>
  <a class="result__snippet" href="x">La Matrix es una película de 1999 dirigida por los hermanos Wachowski.</a>
</div>
<div class="result results_links web-result">
  <h2 class="result__title"><a rel="nofollow" class="result__a"
     href="https://es.wikipedia.org/wiki/Matrix">Duplicado, se descarta</a></h2>
  <a class="result__snippet" href="x">otro resumen</a>
</div>
<div class="result results_links web-result">
  <h2 class="result__title"><a rel="nofollow" class="result__a"
     href="https://www.duckduckgo.com/y.js?ad=1">Anuncio</a></h2>
  <a class="result__snippet" href="x">esto es publicidad</a>
</div>
`

func TestParseHits(t *testing.T) {
	hits := parseHits(fixtureHTML)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, quiere 2 (descarta duplicado y buscador): %+v", len(hits), hits)
	}

	n := hits[0]
	if n.URL != "https://www.netflix.com/es/title/80100172" {
		t.Errorf("no resolvió el enlace redirigido: %q", n.URL)
	}
	if n.Host != "netflix.com" {
		t.Errorf("host = %q", n.Host)
	}
	if n.Platform != "Netflix" || n.Kind != "plataforma" {
		t.Errorf("plataforma = %q / %q", n.Platform, n.Kind)
	}
	if n.Year != "1999" {
		t.Errorf("año = %q", n.Year)
	}
	if !strings.Contains(n.Snippet, "ciencia ficción") {
		t.Errorf("snippet = %q", n.Snippet)
	}
	if strings.Contains(n.Snippet, "<b>") {
		t.Errorf("el snippet debe venir limpio de etiquetas: %q", n.Snippet)
	}
	if n.Favicon == "" {
		t.Error("falta el icono del sitio")
	}

	w := hits[1]
	if w.Kind != "info" || w.Platform != "Wikipedia" {
		t.Errorf("wikipedia = %q / %q", w.Platform, w.Kind)
	}
	if w.Year != "1999" {
		t.Errorf("wikipedia año = %q", w.Year)
	}
}

func TestParseHitsLite(t *testing.T) {
	body := `<table><tr><td class="result-snippet">
	  <a class="result-link" href="https://www.primevideo.com/detail/matrix">Matrix en Prime Video</a>
	  <div class="result-snippet">Ver Matrix online con suscripción.</div></td></tr></table>`
	hits := parseHits(body)
	if len(hits) != 1 || hits[0].Platform != "Prime Video" {
		t.Fatalf("lite = %+v", hits)
	}
	if hits[0].Kind != "plataforma" {
		t.Errorf("kind = %q", hits[0].Kind)
	}
}

func TestUnwrapHostYSkip(t *testing.T) {
	if got := unwrap("//duckduckgo.com/l/?uddg=https://a.com/x&rut=1"); got != "https://a.com/x" {
		t.Errorf("unwrap = %q", got)
	}
	if got := unwrap("https://a.com/directo"); got != "https://a.com/directo" {
		t.Errorf("unwrap directo = %q", got)
	}
	if hostOf("https://WWW.YouTube.com/watch?v=1") != "youtube.com" {
		t.Errorf("hostOf = %q", hostOf("https://WWW.YouTube.com/watch?v=1"))
	}
	for _, h := range []string{"duckduckgo.com", "www.bing.com", "", "google.es"} {
		if !skipHost(h) {
			t.Errorf("%q debería descartarse", h)
		}
	}
	if skipHost("netflix.com") {
		t.Error("netflix.com no debería descartarse")
	}
	if parseHits("") != nil || parseHits("<html><body>nada</body></html>") != nil {
		t.Error("sin resultados debe devolver nil")
	}
}

func TestLookupPlatform(t *testing.T) {
	cases := map[string]platform{
		"www.netflix.com":     {"Netflix", "plataforma"},
		"vimeo.com":           {"Vimeo", "video"},
		"es.wikipedia.org":    {"Wikipedia", "info"},
		"blog.miappesada.com": {},
	}
	for host, want := range cases {
		got, _ := lookupPlatform(host)
		if got != want {
			t.Errorf("lookupPlatform(%q) = %+v, quiere %+v", host, got, want)
		}
	}
}

func TestGuessYear(t *testing.T) {
	if got := guessYear("Blade Runner 2049", ""); got != "2049" {
		t.Errorf("año del título = %q", got)
	}
	if got := guessYear("Matrix", "película de 1999"); got != "1999" {
		t.Errorf("año del resumen = %q", got)
	}
	if got := guessYear("Matrix", "sin año"); got != "" {
		t.Errorf("sin año = %q", got)
	}
}

// ---------- Orden ----------

func hit(title, host, kind, snippet string) Result {
	return Result{Title: title, Host: host, Kind: kind, Snippet: snippet, Platform: host}
}

func TestRankResultsPriorizaLoQueSePuedeVer(t *testing.T) {
	p := parseQuery("matrix")
	in := []Result{
		hit("Matrix", "es.wikipedia.org", "info", "La Matrix es una película de 1999."),
		hit("Matrix | Netflix", "netflix.com", "plataforma", "Mira Matrix en streaming."),
		hit("Noticias del cine", "eluniversal.com.mx", "web", "Hoy parlemos de cultura."),
	}
	got := rankResults(in, p)
	if len(got) != 2 {
		t.Fatalf("resultados = %d: la nota que no habla de la peli se descarta", len(got))
	}
	if got[0].Host != "netflix.com" {
		t.Errorf("primero deberia ser la plataforma, no %q", got[0].Host)
	}
	if got[0].Score <= got[1].Score {
		t.Errorf("plataforma %v no dominate a la enciclopedia %v", got[0].Score, got[1].Score)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Errorf("orden roto: %v antes que %v", got[i-1].Score, got[i].Score)
		}
	}
}

func TestRankResultsUsaElAnio(t *testing.T) {
	p := parseQuery("matrix 1999")
	in := []Result{
		hit("Matrix", "a.com", "web", "Matrix de 2021."),
		hit("Matrix", "b.com", "web", "Matrix de 1999."),
	}
	got := rankResults(in, p)
	if got[0].Title != "b.com" && got[0].Host != "b.com" {
		t.Errorf("debería ganar el año correcto, ganó %q (%v)", got[0].Host, got[0].Score)
	}
}

func TestSortResultsEsEstable(t *testing.T) {
	in := []Result{
		{Title: "b", Score: 50}, {Title: "a", Score: 50}, {Title: "c", Score: 90},
	}
	sortResults(in)
	if in[0].Title != "c" || in[1].Title != "a" || in[2].Title != "b" {
		t.Errorf("orden = %v %v %v", in[0].Title, in[1].Title, in[2].Title)
	}
}

func TestMergeResultsQuitaDuplicados(t *testing.T) {
	in := []Result{
		{URL: "https://a.com/x"}, {URL: "https://a.com/x/"},
		{URL: "https://b.com/y"},
		// misma página en dos países: el prefijo de idioma no la duplica
		{URL: "https://www.justwatch.com/mx/pelicula/matrix"},
		{URL: "https://www.justwatch.com/es/pelicula/matrix"},
	}
	if got := mergeResults(in); len(got) != 3 {
		t.Fatalf("merge = %d, quiere 3: %+v", len(got), got)
	}
}

func TestRankResultsNoSaturaLaPuntuacion(t *testing.T) {
	p := parseQuery("matrix")
	got := rankResults([]Result{
		{Title: "Matrix", Host: "es.wikipedia.org", Kind: "info", Snippet: "La Matrix es una película."},
		{Title: "Matrix", Host: "netflix.com", Kind: "plataforma", Snippet: "Mira Matrix en streaming."},
	}, p)
	if len(got) != 2 {
		t.Fatalf("resultados = %d", len(got))
	}
	if got[0].Score <= got[1].Score {
		t.Fatalf("todo empata a %v: el orden no dice nada", got[0].Score)
	}
	if got[0].Score == got[1].Score {
		t.Fatalf("las dos fichas siguen empatadas: %v", got[0].Score)
	}
}

// ---------- Caché ----------

func TestCacheSirveYExpira(t *testing.T) {
	cacheFlush()
	cacheSet("k", []Result{{Title: "a"}}, time.Minute, false)
	if rs, ok := cacheGet("k", false); !ok || len(rs) != 1 {
		t.Fatal("la caché no devolvió lo guardado")
	}
	cacheSet("v", []Result{{Title: "b"}}, time.Millisecond, false)
	time.Sleep(5 * time.Millisecond)
	if _, ok := cacheGet("v", false); ok {
		t.Fatal("un TTL vencido no debe servir")
	}
	if _, ok := cacheStale("v"); !ok {
		t.Fatal("el respaldo debe servir datos viejos aunque el TTL venz")
	}
}

func TestCacheParcialYNegativo(t *testing.T) {
	cacheFlush()
	cacheSet("p", []Result{{Title: "a"}}, time.Minute, true)
	if _, ok := cacheGet("p", false); ok {
		t.Error("una búsqueda rápida no debe valer para una completa")
	}
	if _, ok := cacheGet("p", true); !ok {
		t.Error("debe valer para una rápida")
	}
	// Una completa manda sobre una rápida ya guardada.
	cacheSet("p", []Result{{Title: "a"}}, time.Minute, false)
	cacheSet("p", []Result{{Title: "corto"}}, time.Minute, true)
	if rs, _ := cacheGet("p", false); len(rs) != 1 || rs[0].Title != "a" {
		t.Errorf("una rápida no debe pisar a la completa: %+v", rs)
	}
}

func TestCacheEviction(t *testing.T) {
	cacheFlush()
	for i := 0; i < cacheMax+10; i++ {
		cacheSet("k"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26)), []Result{{Title: "a"}}, time.Minute, false)
	}
	cacheMu.Lock()
	n := cacheLRU.Len()
	cacheMu.Unlock()
	if n > cacheMax {
		t.Errorf("la caché creció a %d, máximo %d", n, cacheMax)
	}
}

func TestFetchSharedAgrupaPeticiones(t *testing.T) {
	var calls int
	var mu sync.Mutex
	fetch := func(ctx context.Context) ([]Result, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		return []Result{{Title: "única"}}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs, err := fetchShared(context.Background(), "compartida", fetch)
			if err != nil || len(rs) != 1 {
				t.Errorf("resultado inesperado: %+v %v", rs, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Errorf("se hicieron %d peticiones, quiere 1", calls)
	}
}

// ---------- Servidor falso del buscador ----------

// fakeEngine imitates el buscador: devuelve el HTML de ejemplo y cuenta las
// consultas que recibió.
type fakeEngine struct {
	srv     *httptest.Server
	queries []string
	mu      sync.Mutex
	fail    bool
}

func newFakeEngine(t *testing.T, htmlBody, suggestBody string) *fakeEngine {
	t.Helper()
	f := &fakeEngine{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.Query().Get("q"))
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if strings.Contains(r.URL.Path, "ac") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, suggestBody)
			return
		}
		io.WriteString(w, htmlBody)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEngine) point(t *testing.T) {
	t.Helper()
	oldHTML, oldLite, oldSug := ddgHTML, ddgLite, ddgSuggest
	ddgHTML, ddgLite, ddgSuggest = f.srv.URL+"/html", f.srv.URL+"/lite", f.srv.URL+"/ac"
	cacheFlush()
	t.Cleanup(func() { ddgHTML, ddgLite, ddgSuggest = oldHTML, oldLite, oldSug })
}

func TestSearchHandlerDevuelveResultados(t *testing.T) {
	f := newFakeEngine(t, fixtureHTML, `["matrix 1999","matrix resurrecciones"]`)
	f.point(t)

	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=matrix", nil))
	if rec.Code != 200 {
		t.Fatalf("código %d: %s", rec.Code, rec.Body)
	}
	var sr SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Query != "matrix" || len(sr.Results) == 0 {
		t.Fatalf("respuesta = %+v", sr)
	}
	if sr.Results[0].Platform != "Netflix" {
		t.Errorf("primero = %+v", sr.Results[0])
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Error("falta Cache-Control")
	}

	// La segunda vez debe salir de la caché sin preguntar al buscador.
	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=matrix", nil))
	if rec.Code != 200 {
		t.Fatalf("código %d en la repetida", rec.Code)
	}
	f.mu.Lock()
	n := len(f.queries)
	f.mu.Unlock()
	if n < 2 {
		t.Errorf("solo %d consultas al buscador: la segunda debió salir de caché", n)
	}
}

func TestSearchHandlerUsaLaCache(t *testing.T) {
	f := newFakeEngine(t, fixtureHTML, `[]`)
	f.point(t)
	searchHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/search?q=matrix", nil))
	f.mu.Lock()
	antes := len(f.queries)
	f.mu.Unlock()
	searchHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/search?q=matrix", nil))
	f.mu.Lock()
	despues := len(f.queries)
	f.mu.Unlock()
	if despues != antes {
		t.Errorf("la caché no evitó la consulta: %d -> %d", antes, despues)
	}
}

func TestSearchHandlerLimitaResultados(t *testing.T) {
	f := newFakeEngine(t, fixtureHTML, `[]`)
	f.point(t)
	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=matrix&limit=1", nil))
	var sr SearchResponse
	json.Unmarshal(rec.Body.Bytes(), &sr)
	if len(sr.Results) != 1 {
		t.Errorf("resultados = %d, quiere 1", len(sr.Results))
	}
	// Nunca por encima del máximo, aunque pidan 9999.
	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=matrix&limit=9999", nil))
	json.Unmarshal(rec.Body.Bytes(), &sr)
	if len(sr.Results) > maxLimit {
		t.Errorf("resultados = %d, máximo %d", len(sr.Results), maxLimit)
	}
}

func TestSearchHandlerConsultaVariasVeces(t *testing.T) {
	f := newFakeEngine(t, fixtureHTML, `[]`)
	f.point(t)
	searchHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/search?q=matrix", nil))
	f.mu.Lock()
	f.queries = nil
	f.mu.Unlock()
	cacheFlush()
	searchHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/search?q=matrix", nil))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) < 2 {
		t.Errorf("debería probar varias consultas, hizo %d: %v", len(f.queries), f.queries)
	}
	if f.queries[0] != "matrix" {
		t.Errorf("la primera consulta debe ser el título limpio: %q", f.queries[0])
	}
}

func TestSearchHandlerVacioYOPTIONS(t *testing.T) {
	f := newFakeEngine(t, fixtureHTML, `[]`)
	f.point(t)
	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=", nil))
	var sr SearchResponse
	json.Unmarshal(rec.Body.Bytes(), &sr)
	if len(sr.Results) != 0 {
		t.Errorf("consulta vacía = %+v", sr.Results)
	}
	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("OPTIONS", "/api/search?q=matrix", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS = %d", rec.Code)
	}
}

func TestSearchHandlerErrorDelBuscador(t *testing.T) {
	f := newFakeEngine(t, fixtureHTML, `[]`)
	f.point(t)
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	cacheFlush()
	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=matrix", nil))
	if rec.Code != 400 && rec.Code != 502 && rec.Code != 504 {
		t.Errorf("código %d: %s", rec.Code, rec.Body)
	}
}

func TestParseSuggestions(t *testing.T) {
	// Formato plano: lista de objetos.
	got := parseSuggestions(`[{"phrase":"matrix"},{"phrase":"matrix login"}]`, 5)
	if len(got) != 2 || got[0] != "matrix" {
		t.Errorf("formato plano = %+v", got)
	}
	// Formato agrupado: ["consulta", [ ... ]]
	got = parseSuggestions(`["matr",["matrix","matrix login","","matrix 1999"]]`, 5)
	if len(got) != 3 || got[0] != "matrix" {
		t.Errorf("formato agrupado = %+v", got)
	}
	// Respuestas que no son sugerencias.
	if parseSuggestions(`[]`, 5) != nil || parseSuggestions(`"nope"`, 5) != nil {
		t.Error("una respuesta sin sugerencias debe devolver nil")
	}
	// Tope respetado.
	if got = parseSuggestions(`[{"phrase":"a"},{"phrase":"b"},{"phrase":"c"}]`, 2); len(got) != 2 {
		t.Errorf("no respeta el tope: %+v", got)
	}
}

func TestSuggestHandler(t *testing.T) {
	f := newFakeEngine(t, fixtureHTML, `[{"phrase":"matrix 1999"},{"phrase":"matrix resurrecciones"}]`)
	f.point(t)

	rec := httptest.NewRecorder()
	suggestHandler(rec, httptest.NewRequest("GET", "/api/suggest?q=mat", nil))
	var out struct {
		Query   string   `json:"query"`
		Results []string `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || out.Results[0] != "matrix 1999" {
		t.Errorf("sugerencias = %+v", out.Results)
	}

	// Muy corto: responde vacío sin preguntar.
	f.mu.Lock()
	antes := len(f.queries)
	f.mu.Unlock()
	rec = httptest.NewRecorder()
	suggestHandler(rec, httptest.NewRequest("GET", "/api/suggest?q=a", nil))
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Results) != 0 {
		t.Errorf("sugerencias = %+v", out.Results)
	}
	f.mu.Lock()
	despues := len(f.queries)
	f.mu.Unlock()
	if despues != antes {
		t.Error("una letra no debería gastar una consulta")
	}
}

// TestCompressGzipYDocsPages evita el fallo más caro del proyecto: comprimir la
// respuesta sin mandar Content-Encoding hacía que el navegador viera binario gzip
// en lugar del HTML.
func TestCompressGzipYDocsPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/resultados", pageHandler("/resultados", "catalog.html"))
	mux.HandleFunc("/resultados/", pageHandler("/resultados", "catalog.html"))
	mux.HandleFunc("/", homeHandler)
	h := compress(mux)

	for _, path := range []string{"/", "/resultados", "/resultados/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Fatalf("%s sin gzip: %d %q", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("%s Content-Type: %q", path, ct)
		}

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

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("HEAD", "/", nil))
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("HEAD comprimido: %q", got)
	}
}

// TestPagesEnlazadas verifica que el launcher y los resultados se alcanzan entre sí.
func TestPagesEnlazadas(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/resultados", pageHandler("/resultados", "catalog.html"))
	mux.HandleFunc("/resultados/", pageHandler("/resultados", "catalog.html"))
	mux.HandleFunc("/", homeHandler)

	for _, path := range []string{"/", "/resultados", "/resultados/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s debería servir la página, dio %d", path, rec.Code)
		}
	}

	home, err := site.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := site.ReadFile("catalog.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(home), `href="/resultados"`) || !strings.Contains(string(home), `link.href = 'catalog.html'`) {
		t.Error("index.html no lleva a los resultados (ni cae al archivo)")
	}
	if !strings.Contains(string(cat), `href="/"`) || !strings.Contains(string(cat), `link.href = 'index.html'`) {
		t.Error("catalog.html no vuelve al launcher (ni cae al archivo)")
	}
}

// TestIndexTieneElBuscador comprueba que el launcher es, sobre todo, un buscador.
func TestIndexTieneElBuscador(t *testing.T) {
	home, err := site.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := site.ReadFile("catalog.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id=\"q\"", "/api/suggest", "/resultados", "'?q='"} {
		if !strings.Contains(string(home), want) {
			t.Errorf("index.html no tiene %s", want)
		}
	}
	if strings.Contains(string(home), "TMDB") || strings.Contains(string(home), "tmdb") {
		t.Error("el launcher ya no debe mentionar TMDB")
	}
	if !strings.Contains(string(cat), "/api/search") {
		t.Error("catalog.html debe pedir resultados a /api/search")
	}
}
