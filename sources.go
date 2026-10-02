package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- Motor multíndice ----------
//
// Un solo buscador solo ve lo que él ha rastreado. Preguntando a varios a la vez
// y juntando lo que devuelven, la respuesta llega a páginas que ninguno habría
// encontrado por su cuenta. Ninguna fuente es imprescindible: la que falle,
// pida captcha o tarde se salta y la búsqueda sigue con las demás.

const (
	sourceTimeout = 5 * time.Second // por índice; el total lo manda requestTimout
	maxPerHost    = 2               // ninguna plataforma se lleva la pantalla entera
	votePoints    = 7               // cada índice independiente que coincide suma
)

type source struct {
	name  string
	limit int
	fetch func(ctx context.Context, queries []string) ([]Result, error)
}

// webSources son los índices que se consultan en paralelo. Los que piden
// captcha sin clé (Mojeek, Ecosia, Startpage, Bing…) no están: cuando están
// bloqueados solo suman espera y su formato cambia sin avisar. Brave sí puede
// entrar, con clave.
var webSources = []source{
	{name: "DuckDuckGo", limit: maxHits, fetch: duckSource},
	{name: "Wikipedia", limit: 8, fetch: wikiSource},
	{name: "Internet Archive", limit: 8, fetch: archiveSource},
	{name: "Wiby", limit: 6, fetch: wibySource},
}

// braveSource entra en el registro solo si hay clave. Con ella el buscador no
// depende de que un tercero nos deje pasar y no hay captchas.
// La clave gratuita (2000 búsquedas al mes) se saca en api-dashboard.search.brave.com.
func braveSource(ctx context.Context, queries []string) ([]Result, error) {
	if braveKey == "" {
		return nil, errNoEngine
	}
	if len(queries) == 0 {
		return nil, errNoEngine
	}
	u, err := url.Parse(braveAPI)
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"q": {queries[0]}, "count": {"20"}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", braveKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errNoEngine
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, errNoEngine
	}
	out := make([]Result, 0, len(payload.Web.Results))
	for _, r := range payload.Web.Results {
		host := hostOf(r.URL)
		if r.Title == "" || skipHost(host) {
			continue
		}
		res := Result{
			Title: trimRunes(r.Title, 140), URL: r.URL, Host: host,
			Snippet: trimRunes(cleanText(r.Description), 260), Source: "Brave",
			Kind: "web", Favicon: faviconURL(host), Year: guessYear(r.Title, r.Description),
		}
		if p, ok := lookupPlatform(host); ok {
			res.Platform, res.Kind = p.name, p.kind
		}
		out = append(out, res)
	}
	if len(out) == 0 {
		return nil, errNoEngine
	}
	return out, nil
}

// Puntos de entrada de cada índice. Son variables para que las pruebas puedan
// apuntarlos a un servidor falso y no salir a internet.
var (
	wikiAPI    = "https://es.wikipedia.org/w/api.php"
	wikiSite   = "https://es.wikipedia.org/wiki/"
	wikiLang1  = "es"
	wikiLang2  = "en"
	archiveAPI = "https://archive.org/advancedsearch.php"
	wibySearch = "https://wiby.me/"
	braveAPI   = "https://api.search.brave.com/res/v1/web/search"
)

// braveKey se lee del entorno al arrancar. Sin ella, Brave no entra en el
// registro y el buscador funciona igual con los índices gratuitos.
var braveKey = os.Getenv("BRAVE_API_KEY")

func init() {
	if braveKey != "" {
		webSources = append([]source{{name: "Brave", limit: maxHits, fetch: braveSource}}, webSources...)
	}
}

// searchAll pregunta a todos los índices a la vez y fusiona lo que devuelven.
// Devuelve también de qué índices salieron resultados, para poder decirlo.
func searchAll(ctx context.Context, p parsed) ([]Result, []string, error) {
	queries := p.webQueries()

	collected := make([][]Result, len(webSources))
	var wg sync.WaitGroup
	for i, s := range webSources {
		wg.Add(1)
		go func(i int, s source) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
			defer cancel()
			rs, err := s.fetch(sctx, queries)
			if err != nil || len(rs) == 0 {
				return
			}
			if s.limit > 0 && len(rs) > s.limit {
				rs = rs[:s.limit]
			}
			collected[i] = rs
		}(i, s)
	}
	wg.Wait()

	var (
		all   []Result
		names []string
	)
	for i, rs := range collected {
		if len(rs) == 0 {
			continue
		}
		names = append(names, webSources[i].name)
		all = append(all, rs...)
	}
	if len(all) == 0 {
		return nil, nil, errNoEngine
	}
	return voteResults(all, names), names, nil
}

// voteResults junta lo que han dicho los índices. Si dos rastreos independientes
// señalan la misma página, es mucho más probable que sea la que se busca, así que
// el acuerdo se cuenta y luego puntúa.
func voteResults(all []Result, names []string) []Result {
	var out []Result
	for _, r := range all {
		merged := false
		for i := range out {
			// Misma URL, o mismo sitio con el mismo título: los buscadores
			// devuelven la página varias veces con parámetros distintos.
			mismo := samePage(r.URL, out[i].URL) ||
				(r.Host == out[i].Host && fold(cleanTitle(r.Title)) == fold(cleanTitle(out[i].Title)))
			if !mismo {
				continue
			}
			out[i].Votes++
			// la descripción más larga suele venir del índice que mejor la escreve
			if len(r.Snippet) > len(out[i].Snippet) {
				out[i].Snippet = r.Snippet
			}
			merged = true
			break
		}
		if merged {
			continue
		}
		r.Votes = 1
		out = append(out, r)
	}
	return out
}

// ---------- Índices ----------

// httpGet pide una URL y devuelve el cuerpo. Cualquier estado que no sea 200
// (un captcha incluido) cuenta como fallo de esa fuente.
func httpGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "es,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errNoEngine
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ---------- El buscador principal tiene que respirar ----------

// DuckDuckGo contesta con un desafío cuando cree que le llegan demasiadas
// peticiones. Insistir solo alarga el castigo, así que cuando pasa se le deja
// descansar: la búsqueda la sostienen los otros índices mientras tanto.

var (
	breakerMu     sync.Mutex
	blockedUntil  time.Time
	blockedStreak int
	lastRequest   time.Time
	ddgGap        = 1200 * time.Millisecond
)

// ddgReset deja el buscador como recién arrancado. Lo usan las pruebas.
func ddgReset() {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	blockedStreak, blockedUntil, lastRequest = 0, time.Time{}, time.Time{}
}

// ddgQuiet dice si hay que dejar descansar al buscador.
func ddgQuiet() bool {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	return time.Now().Before(blockedUntil)
}

// ddgBlocked anota un desafío y aparta el buscador un rato, que crece si insiste.
func ddgBlocked() {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	blockedStreak++
	wait := time.Duration(blockedStreak) * 5 * time.Minute
	if wait > 30*time.Minute {
		wait = 30 * time.Minute
	}
	blockedUntil = time.Now().Add(wait)
}

// ddgOK marca que el buscador volvió a responder con calma.
func ddgOK() {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	blockedStreak = 0
	blockedUntil = time.Time{}
}

// waitTurn separa las peticiones al buscador: ráfagas seguidas son justo lo que
// loProvoca el bloqueo.
func waitTurn(ctx context.Context) error {
	breakerMu.Lock()
	since := time.Since(lastRequest)
	if since < ddgGap {
		breakerMu.Unlock()
		select {
		case <-time.After(ddgGap - since):
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		breakerMu.Unlock()
	}
	breakerMu.Lock()
	lastRequest = time.Now()
	breakerMu.Unlock()
	return nil
}

// duckSource busca en el buscador abierto con varias formulaciones: el título
// solo, con año y buscando algo que se pueda ver. Si el HTML principal pide
// captcha, la versión "lite" suele escapar.
func duckSource(ctx context.Context, queries []string) ([]Result, error) {
	if ddgQuiet() {
		return nil, errNoEngine // descansa: los otros índices cubren la búsqueda
	}
	// Con más índices, dos consultas bastan: pedir más solo multiplica las
	// peticiones y hace que el buscador se cierre antes.
	if len(queries) > 2 {
		queries = queries[:2]
	}
	var (
		all   []Result
		last  error
		asked bool
	)
	for _, q := range queries {
		if err := waitTurn(ctx); err != nil {
			break
		}
		asked = true
		body, err := fetchHTML(ctx, ddgHTML, q)
		if err != nil {
			last = err
			if body, err = fetchHTML(ctx, ddgLite, q); err != nil {
				last = err
				continue
			}
		}
		all = append(all, parseHits(body)...)
		if len(all) >= maxHits {
			break
		}
	}
	if len(all) == 0 {
		if asked {
			ddgBlocked()
		}
		if last != nil {
			return nil, last
		}
		return nil, errNoEngine
	}
	ddgOK()
	return mergeResults(all), nil
}

// wikiSource busca la ficha del título: es donde está el nombre oficial, el año
// y los títulos alternativos, así que ayuda a reconocer la película aunque el
// usuario la haya escrito mal o en el otro idioma.
func wikiSource(ctx context.Context, queries []string) ([]Result, error) {
	if len(queries) == 0 {
		return nil, errNoEngine
	}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out []Result
	)
	for _, lang := range []string{wikiLang1, wikiLang2} {
		wg.Add(1)
		go func(lang string) {
			defer wg.Done()
			rs, err := wikiLang(ctx, lang, queries[0])
			if err != nil {
				return
			}
			mu.Lock()
			out = append(out, rs...)
			mu.Unlock()
		}(lang)
	}
	wg.Wait()
	if len(out) == 0 {
		return nil, errNoEngine
	}
	return out, nil
}

func wikiLang(ctx context.Context, lang, query string) ([]Result, error) {
	api := strings.ReplaceAll(wikiAPI, wikiLang1, lang)
	u, err := url.Parse(api)
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{
		"action": {"query"}, "list": {"search"}, "srsearch": {query},
		"srlimit": {"4"}, "format": {"json"},
	}.Encode()

	body, err := httpGet(ctx, u.String())
	if err != nil {
		return nil, err
	}
	var payload struct {
		Query struct {
			Search []struct {
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errNoEngine
	}
	site := strings.ReplaceAll(wikiSite, wikiLang1, lang)
	out := make([]Result, 0, len(payload.Query.Search))
	for _, w := range payload.Query.Search {
		host := hostOf(site)
		link := site + strings.ReplaceAll(url.PathEscape(w.Title), "%2F", "/")
		out = append(out, Result{
			Title: trimRunes(w.Title, 140), URL: link, Host: host,
			Snippet: trimRunes(cleanText(w.Snippet), 260), Source: "Wikipedia",
			Kind: "info", Favicon: faviconURL(host), Year: guessYear(w.Title, w.Snippet),
		})
	}
	if len(out) == 0 {
		return nil, errNoEngine
	}
	return out, nil
}

// flexYear lee el año de Archive, que a veces viene como número y otras como texto.
type flexYear int

func (y *flexYear) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil // un año ilegible no es motivo para perder el resultado
	}
	*y = flexYear(n)
	return nil
}

// archiveSource busca en el archivo de películas de dominio público. A diferencia
// del resto, estos resultados se pueden ver ahí mismo.
func archiveSource(ctx context.Context, queries []string) ([]Result, error) {
	if len(queries) == 0 {
		return nil, errNoEngine
	}
	u, err := url.Parse(archiveAPI)
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{
		"q":      {fmt.Sprintf("title:(%s) AND mediatype:movies", queries[0])},
		"fl[]":   {"identifier", "title", "year"},
		"rows":   {"8"},
		"output": {"json"},
		"page":   {"1"},
	}.Encode()

	body, err := httpGet(ctx, u.String())
	if err != nil {
		return nil, err
	}
	var payload struct {
		Response struct {
			Docs []struct {
				Identifier string   `json:"identifier"`
				Title      string   `json:"title"`
				Year       flexYear `json:"year"`
			} `json:"docs"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errNoEngine
	}
	out := make([]Result, 0, len(payload.Response.Docs))
	for _, d := range payload.Response.Docs {
		// Archive cataloga también vídeos subidos a YouTube como si fueran
		// películas: no es la película.
		if strings.HasPrefix(strings.ToLower(d.Identifier), "youtube-") {
			continue
		}
		link := "https://archive.org/details/" + url.PathEscape(d.Identifier)
		title := strings.TrimSpace(d.Title)
		if title == "" {
			title = d.Identifier
		}
		snip := "Película de dominio público, se puede ver en el archivo"
		if d.Year > 0 {
			snip = fmt.Sprintf("Película de %d, se puede ver en el archivo", d.Year)
		}
		out = append(out, Result{
			Title: trimRunes(title, 140), URL: link, Host: "archive.org",
			Snippet: snip, Source: "Internet Archive", Kind: "video",
			Favicon: faviconURL("archive.org"), Year: guessYear(title, snip),
		})
	}
	if len(out) == 0 {
		return nil, errNoEngine
	}
	return out, nil
}

// wibySource consulta un índice independiente y pequeño: páginas personales y
// sitios olvidados. Nada de lo que encuentra es una plataforma grande, pero es
// donde se guarda lo que los grandes ya no indexan.
var (
	reWibyBlock = regexp.MustCompile(`(?is)<blockquote[^>]*>(.*?)</blockquote>`)
	reWibyLink  = regexp.MustCompile(`(?is)<a[^>]+class="tlink"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reWibySnip  = regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
)

func wibySource(ctx context.Context, queries []string) ([]Result, error) {
	if len(queries) == 0 {
		return nil, errNoEngine
	}
	u, err := url.Parse(wibySearch)
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"q": {queries[0]}}.Encode()

	body, err := httpGet(ctx, u.String())
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, block := range reWibyBlock.FindAllStringSubmatch(string(body), -1) {
		link := reWibyLink.FindStringSubmatch(block[1])
		if link == nil {
			continue
		}
		title := cleanText(link[2])
		host := hostOf(link[1])
		if title == "" || skipHost(host) {
			continue
		}
		snippet := ""
		if p := reWibySnip.FindStringSubmatch(block[1]); p != nil {
			snippet = cleanText(p[1])
		}
		out = append(out, Result{
			Title: trimRunes(title, 140), URL: link[1], Host: host,
			Snippet: trimRunes(snippet, 260), Source: "Wiby",
			Kind: "web", Favicon: faviconURL(host), Year: guessYear(title, snippet),
		})
	}
	if len(out) == 0 {
		return nil, errNoEngine
	}
	return out, nil
}

// ---------- Sugerencias de todos los buscadores ----------

// suggestSources son los autocompletados que responden sin clave. Se consultan
// todos y se mezclan, porque cada uno sabe de cosas distintas: Brave conoce
// títulos, Google lo que se busca en español y los demás añaden variantes.
type suggestSource struct {
	name string
	url  string // con %s donde va la consulta
}

var suggestSources = []suggestSource{
	{"DuckDuckGo", "https://duckduckgo.com/ac/?q=%s"},
	{"Google", "https://suggestqueries.google.com/complete/search?client=firefox&hl=es&q=%s"},
	{"Brave", "https://search.brave.com/api/suggest?q=%s"},
	{"Bing", "https://api.bing.com/osjson.aspx?query=%s"},
	{"Yahoo", "https://search.yahoo.com/sugg/gossip/gossip-us-ura/?output=sd1&command=%s"},
	{"Yandex", "https://yandex.com/suggest/suggest-ya.cgi?v=4&part=%s"},
}

// suggestAll pregunta a todos los autocompletados a la vez. Se turnan para que
// ninguno se lleve todas las plazas: el primero que responde algo pone el título
// exacto y los demás rellenan con lo que aporten.
func suggestAll(ctx context.Context, q string) ([]string, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	lists := make([][]string, len(suggestSources))
	var wg sync.WaitGroup
	for i, s := range suggestSources {
		wg.Add(1)
		go func(i int, s suggestSource) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
			defer cancel()
			body, err := httpGet(sctx, fmt.Sprintf(s.url, url.QueryEscape(q)))
			if err != nil {
				return
			}
			lists[i] = parseSuggestBody(string(body), 10)
		}(i, s)
	}
	wg.Wait()

	// Cuantos más buscadores proponen la misma frase, más gente la escribe: eso
	// vale más que el puesto que ocupa en la lista de uno solo.
	type vote struct {
		text     string
		howMany  int
		bestRank int
	}
	votes := map[string]*vote{}
	for _, list := range lists {
		for rank, s := range list {
			s = strings.TrimSpace(s)
			key := strings.ToLower(s)
			if s == "" || !latinOnly(s) || !typedOnQuery(q, s) {
				continue
			}
			if v, ok := votes[key]; ok {
				v.howMany++
				if rank < v.bestRank {
					v.bestRank, v.text = rank, s
				}
				continue
			}
			votes[key] = &vote{text: s, howMany: 1, bestRank: rank}
		}
	}
	all := make([]vote, 0, len(votes))
	for _, v := range votes {
		all = append(all, *v)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].howMany != all[j].howMany {
			return all[i].howMany > all[j].howMany
		}
		if all[i].bestRank != all[j].bestRank {
			return all[i].bestRank < all[j].bestRank
		}
		return all[i].text < all[j].text
	})

	out := make([]string, 0, suggestLimit)
	for _, v := range all {
		out = append(out, trimRunes(v.text, 80))
		if len(out) == suggestLimit {
			break
		}
	}
	if len(out) == 0 {
		return nil, errNoEngine
	}
	return out, nil
}

// reCyrillic detecta alfabetos que no son el nuestro: Yandex responde con
// sugerencias en ruso o turco que aquí no sirven de nada.
var reCyrillic = regexp.MustCompile(`[\x{0400}-\x{04FF}]`)

func latinOnly(s string) bool { return !reCyrillic.MatchString(s) }

// typedOnQuery descarta lo que no tiene nada que ver con lo tecleado: los
// buscadores devuelven de todo cuando la palabra es corta.
func typedOnQuery(q, s string) bool {
	return strings.HasPrefix(fold(s), fold(q))
}

// parseSuggestBody acepta los formatos de los distintos buscadores: la lista
// doble ["matrix",["matrix",...]] y el objeto de Yahoo {"r":[{"k":"matrix"}]}.
func parseSuggestBody(body string, max int) []string {
	var out []string
	var yahoo struct {
		R []struct {
			K string `json:"k"`
		} `json:"r"`
	}
	if err := json.Unmarshal([]byte(body), &yahoo); err == nil && len(yahoo.R) > 0 {
		for _, r := range yahoo.R {
			if r.K = strings.TrimSpace(r.K); r.K != "" {
				out = append(out, trimRunes(r.K, 80))
			}
			if len(out) == max {
				return out
			}
		}
		return out
	}
	return parseSuggestions(body, max)
}
