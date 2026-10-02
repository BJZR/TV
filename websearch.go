package main

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Motor de búsqueda: consulta la web abierta y devuelve páginas donde aparece
// la película. No depende de ninguna base de datos de películas ni de API con clave.

const (
	webTimeout = 7 * time.Second
	maxHits    = 60
	userAgent  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
)

// Puntos de entrada del buscador. Son variables para que las pruebas puedan
// apuntarlas a un servidor falso y no salir a internet.
var (
	ddgHTML    = "https://html.duckduckgo.com/html/"
	ddgLite    = "https://lite.duckduckgo.com/lite/"
	ddgSuggest = "https://duckduckgo.com/ac/"
)

var errNoEngine = errors.New("no se pudo consultar el buscador")

// platform identifica webs conocidas de streaming para etiquetar el resultado,
// igual que hace Seekee al decir en qué plataforma está cada título.
type platform struct {
	name string
	kind string // "plataforma" | "video"
}

var platforms = []struct {
	host string
	plat platform
}{
	{"netflix.com", platform{"Netflix", "plataforma"}},
	{"disneyplus.com", platform{"Disney+", "plataforma"}},
	{"primevideo.com", platform{"Prime Video", "plataforma"}},
	{"video.amazon.", platform{"Prime Video", "plataforma"}},
	{"max.com", platform{"Max", "plataforma"}},
	{"hbomax", platform{"Max", "plataforma"}},
	{"appletv.apple.com", platform{"Apple TV", "plataforma"}},
	{"tv.apple.com", platform{"Apple TV", "plataforma"}},
	{"paramountplus.com", platform{"Paramount+", "plataforma"}},
	{"peacocktv.com", platform{"Peacock", "plataforma"}},
	{"vix.com", platform{"ViX", "plataforma"}},
	{"crunchyroll.com", platform{"Crunchyroll", "plataforma"}},
	{"mubi.com", platform{"MUBI", "plataforma"}},
	{"movistarplus", platform{"Movistar Plus+", "plataforma"}},
	{"movistarplay", platform{"Movistar Plus+", "plataforma"}},
	{"pluto.tv", platform{"Pluto TV", "plataforma"}},
	{"rakuten.tv", platform{"Rakuten TV", "plataforma"}},
	{"tubi.com", platform{"Tubi", "plataforma"}},
	{"roku.com", platform{"Roku", "plataforma"}},
	{"plex.tv", platform{"Plex", "plataforma"}},
	{"themoviedb.org", platform{"TMDB", "info"}},
	{"wikipedia.org", platform{"Wikipedia", "info"}},
	{"filmaffinity.com", platform{"FilmAffinity", "info"}},
	{"letterboxd.com", platform{"Letterboxd", "info"}},
	{"imdb.com", platform{"IMDb", "info"}},
	{"youtube.com", platform{"YouTube", "video"}},
	{"youtu.be", platform{"YouTube", "video"}},
	{"vimeo.com", platform{"Vimeo", "video"}},
	{"archive.org", platform{"Archive.org", "video"}},
	{"cuevana", platform{"Cuevana", "video"}},
	{"splayer", platform{"SPlayer", "video"}},
}

// lookupPlatform devuelve la plataforma de un host y si es el primer dominio
// exacto, para no confundir "vix.com" con cualquier subdominio.
func lookupPlatform(host string) (platform, bool) {
	h := strings.ToLower(host)
	h = strings.TrimPrefix(h, "www.")
	for _, p := range platforms {
		if h == p.host || strings.HasSuffix(h, "."+p.host) || strings.Contains(h, p.host) {
			return p.plat, true
		}
	}
	return platform{}, false
}

// faviconURL usa el servicio de iconos de Google como el launcher.
func faviconURL(host string) string {
	if host == "" {
		return ""
	}
	return "https://www.google.com/s2/favicons?sz=64&domain=" + url.QueryEscape(host)
}

// ---------- Lectura del HTML del buscador ----------

var (
	reStripTags = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces    = regexp.MustCompile(`\s+`)

	// html.duckduckgo.com: <a class="result__a" href="..">Título</a>
	reResultA = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*\bresult__a\b[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	// y el resumen que va justo detrás del enlace.
	reResultSnippet = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*\bresult__snippet\b[^"]*"[^>]*>(.*?)</a>`)
	// variante "lite" del mismo buscador.
	reLiteTD = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result-link[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reLiteSn = regexp.MustCompile(`(?is)<td[^>]+class="[^"]*result-snippet[^"]*"[^>]*>(.*?)</td>`)
)

func cleanText(s string) string {
	s = reStripTags.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
}

// unwrap resuelve el enlace intermedio del buscador: /l/?uddg=<url real>.
func unwrap(raw string) string {
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if q := u.Query().Get("uddg"); q != "" {
		return q
	}
	if q := u.Query().Get("u"); strings.HasPrefix(q, "http") {
		if d, err := url.QueryUnescape(q); err == nil {
			return d
		}
	}
	return raw
}

// hostOf devuelve el dominio sin www.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.ToLower(u.Host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.TrimPrefix(h, "www.")
}

// skipHost descarta los enlaces que no son contenido: el propio buscador,
// validadores, redes sociales y anclas internas.
func skipHost(h string) bool {
	if h == "" {
		return true
	}
	for _, bad := range []string{"duckduckgo.", "bing.", "google.", "search.yahoo", "yandex.", "baidu."} {
		if strings.Contains(h, bad) {
			return true
		}
	}
	return false
}

// cleanTitle quita el adorno que ponen los sitios al final del título:
// "Matrix | Netflix", "The Matrix - Wikipedia"...
func cleanTitle(s string) string {
	if i := strings.IndexAny(s, "|-–—»"); i > 0 {
		if left := strings.TrimSpace(s[:i]); left != "" {
			s = left
		}
	}
	return strings.TrimSpace(s)
}

// parseHits extrae los resultados de las dos variantes de HTML del buscador.
func parseHits(body string) []Result {
	if body == "" {
		return nil
	}
	links, snips := reResultA.FindAllStringSubmatch(body, -1), reResultSnippet.FindAllStringSubmatch(body, -1)
	if len(links) == 0 {
		links, snips = reLiteTD.FindAllStringSubmatch(body, -1), reLiteSn.FindAllStringSubmatch(body, -1)
	}
	if len(links) == 0 {
		return nil
	}

	out := make([]Result, 0, len(links))
	seen := map[string]bool{}
	for i, m := range links {
		link := unwrap(html.UnescapeString(m[1]))
		title := cleanText(m[2])
		host := hostOf(link)
		if title == "" || skipHost(host) || seen[link] {
			continue
		}
		seen[link] = true
		snippet := ""
		if i < len(snips) {
			snippet = cleanText(snips[i][1])
		}
		r := Result{
			Title: trimRunes(title, 140), URL: link, Host: host,
			Snippet: trimRunes(snippet, 260), Source: "DuckDuckGo",
			Kind: "web", Favicon: faviconURL(host), Year: guessYear(title, snippet),
		}
		if p, ok := lookupPlatform(host); ok {
			r.Platform, r.Kind = p.name, p.kind
		}
		out = append(out, r)
		if len(out) >= maxHits {
			break
		}
	}
	return out
}

var reYear = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)

// guessYear saca el año del título o del resumen, que es como los sitios
// escriben la película ("Matrix, 1999").
func guessYear(title, snippet string) string {
	if m := reYear.FindString(title); m != "" {
		return m
	}
	return reYear.FindString(snippet)
}

// ---------- Petición al buscador ----------

func fetchHTML(ctx context.Context, endpoint, query string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	q := url.Values{"q": {query}, "kl": {"wt-wt"}}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/json")
	req.Header.Set("Accept-Language", "es,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errNoEngine
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if err != nil {
		return "", err
	}
	// El buscador contesta con un 202 y una página de "anomaly" cuando decide
	// que le llegan demasiadas peticiones. No es una respuesta vacía: es un
	// portazo, y conviene notarlo para no pedirle la variante lite encima.
	if looksBlocked(string(b)) {
		return "", errNoEngine
	}
	return string(b), nil
}

// looksBlocked detecta el desafío con el que el buscador rechaza al visitante.
// Son las frases literales de su página de captcha: "captcha" a secas
// Appearance sí aparece en títulos de películas y daría falsos positivos.
var blockMarkers = []string{
	"confirm this search was made by a human",
	"bots use duckduckgo too",
}

func looksBlocked(body string) bool {
	head := body
	if len(head) > 4000 {
		head = head[:4000]
	}
	low := strings.ToLower(head)
	for _, mark := range blockMarkers {
		if strings.Contains(low, mark) {
			return true
		}
	}
	return false
}

var client = &http.Client{Timeout: webTimeout}

// reLocale quita el prefijo de país de una ruta: justwatch.com/mx/pelicula/x y
// justwatch.com/es/pelicula/x son la misma película.
var reLocale = regexp.MustCompile(`^/[a-z]{2}(/|$)`)

// samePage decide si dos URL apuntan a la misma página aunque cambien el
// idioma o el país.
func samePage(a, b string) bool {
	// "https://x/pelicula" y "https://x/pelicula/" son la misma página.
	if a, b = strings.TrimSuffix(a, "/"), strings.TrimSuffix(b, "/"); a == b && a != "" {
		return true
	}
	ua, ub := a, b
	if u, err := url.Parse(a); err == nil {
		ua = u.Host + reLocale.ReplaceAllString(u.Path, "$1")
	}
	if u, err := url.Parse(b); err == nil {
		ub = u.Host + reLocale.ReplaceAllString(u.Path, "$1")
	}
	return ua != "" && ua == ub
}

// mergeResults junta las consultas de todas las variantes y quita duplicados.
func mergeResults(all []Result) []Result {
	var out []Result
	for _, r := range all {
		key := strings.TrimSuffix(r.URL, "/")
		dup := false
		for _, prev := range out {
			if samePage(key, prev.URL) || strings.TrimSuffix(prev.URL, "/") == key {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ---------- Sugerencias ----------

// parseSuggestions acepta los dos formatos que contesta el buscador: una lista
// de objetos {"phrase": "..."} y la versión agrupada ["consulta", ["matrix", ...]].
func parseSuggestions(body string, max int) []string {
	var objs []struct {
		Phrase string `json:"phrase"`
	}
	if err := json.Unmarshal([]byte(body), &objs); err == nil && len(objs) > 0 {
		out := make([]string, 0, len(objs))
		for _, o := range objs {
			if p := strings.TrimSpace(o.Phrase); p != "" {
				out = append(out, trimRunes(p, 80))
			}
			if len(out) == max {
				break
			}
		}
		return out
	}

	var group []json.RawMessage
	if err := json.Unmarshal([]byte(body), &group); err != nil {
		return nil
	}
	var out []string
	for _, raw := range group {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			continue // el primer elemento es la consulta repetida
		}
		for _, s := range list {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, trimRunes(s, 80))
			}
			if len(out) == max {
				return out
			}
		}
	}
	return out
}

// ---------- Puntuación ----------

// platformBonus prioriza las páginas donde se puede ver algo: una plataforma de
// streaming vale más que un artículo de wikipedia sobre la peli.
func platformBonus(r Result) float64 {
	switch r.Kind {
	case "plataforma":
		return 20
	case "video":
		return 12
	case "info":
		return -6 // wikipedia, imdb, filmaffinity: sirven, pero no reproducen
	}
	return 0
}

// snippetBonus sube los resultados cuyo resumen promete película: "ver",
// "watch", "streaming", "repelis"...
var reWatch = regexp.MustCompile(`(?i)\b(ver|watch|streaming|reproducir|online|descargar|full ?pel[ií]cula|pelicula completa)\b`)

func snippetBonus(r Result) float64 {
	if reWatch.MatchString(r.Snippet + " " + r.Title) {
		return 6
	}
	return 0
}

// voteBonus sube lo que varios índices señalan a la vez: si dos rastreos
// independientes están de acuerdo, esa página es la que se busca.
func voteBonus(r Result) float64 {
	if r.Votes <= 1 {
		return 0
	}
	return float64(r.Votes-1) * votePoints
}

// capHosts deja como mucho maxPerHost resultados de una misma web. Sin esto,
// las plataformas grandes se llevan la pantalla entera y no se ve nada más.
func capHosts(rs []Result) []Result {
	count := map[string]int{}
	out := make([]Result, 0, len(rs))
	for _, r := range rs {
		if count[r.Host] >= maxPerHost {
			continue
		}
		count[r.Host]++
		out = append(out, r)
	}
	return out
}

// minMatch descarta las páginas que no hablan de la película buscada. Por
// debajo de esto es ruido del buscador, aunque el título coincida en un detalle.
const minMatch = 45

// scoreResult puntúa un resultado contra la consulta (0..100).
func scoreResult(r Result, p parsed) float64 {
	title := cleanTitle(r.Title)
	base := bestText(p.Fold, title, title+" "+r.Host)
	if p.RawFold != p.Fold {
		base = math.Max(base, bestText(p.RawFold, title))
	}
	if p.Year != "" {
		year := r.Year
		if year == "" {
			year = guessYear(r.Title, r.Snippet)
		}
		switch {
		case year == "":
			base -= 4 // sin año no se sabe si es esta
		case year == p.Year:
			base += 16
		default:
			base -= 40 // es otra película: "matrix 1999" no es "Reloaded"
		}
	}
	return math.Max(0, math.Min(100, base))
}

// rankResults ordena por relevancia: primero lo que se parece al título
// buscado y después lo que es una plataforma de streaming.
func rankResults(rs []Result, p parsed) []Result {
	out := make([]Result, 0, len(rs))
	for _, r := range rs {
		// Si el año va en el título y no es el pedido, es otra película con el
		// mismo nombre: "matrix 1999" no debe traer "The Matrix Reloaded".
		if p.Year != "" {
			if y := reYear.FindString(r.Title); y != "" && y != p.Year {
				continue
			}
		}
		r.Match = scoreResult(r, p)
		if r.Match < minMatch {
			continue // no es la película buscada
		}
		// El tope es 140 y no 100 a propósito: si no, todo lo que coincide con
		// el título empata a 100 y el orden deja de significar nada.
		r.Score = math.Round(math.Min(160, r.Match+platformBonus(r)+snippetBonus(r)+voteBonus(r))*10) / 10
		out = append(out, r)
	}
	sortResults(out)
	return capHosts(out)
}

// sortResults ordena por puntuación y, a igualdad, por título para que sea
// estable entre llamadas.
func sortResults(rs []Result) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0; j-- {
			a, b := rs[j-1], rs[j]
			if b.Score > a.Score || (b.Score == a.Score && b.Title < a.Title) {
				rs[j-1], rs[j] = b, a
				continue
			}
			break
		}
	}
}
