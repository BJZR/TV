package main

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Result struct {
	ID            int     `json:"id,omitempty"`
	Type          string  `json:"type"` // movie | tv | live
	Title         string  `json:"title"`
	OriginalTitle string  `json:"originalTitle,omitempty"`
	Year          string  `json:"year,omitempty"`
	Overview      string  `json:"overview,omitempty"`
	Poster        string  `json:"poster,omitempty"`
	Backdrop      string  `json:"backdrop,omitempty"`
	Rating        float64 `json:"rating,omitempty"`
	Votes         int     `json:"votes,omitempty"`
	URL           string  `json:"url,omitempty"`
	Source        string  `json:"source"`
	Via           string  `json:"via,omitempty"`
	Score         float64 `json:"score"`
	Match         float64 `json:"-"`
}

type SearchResponse struct {
	Query      string   `json:"query"`
	Year       string   `json:"year,omitempty"`
	Total      int      `json:"total"`
	Results    []Result `json:"results"`
	DidYouMean string   `json:"didYouMean,omitempty"`
	SpellFixed bool     `json:"spellFixed,omitempty"`
}

// ---------- Caché LRU con TTL y respaldo de datos viejos ----------

type cacheItem struct {
	key     string
	cands   []*candidate
	at      time.Time
	ttl     time.Duration
	partial bool // solo búsqueda rápida: vale menos
}

var (
	cacheMu  sync.Mutex
	cacheMap = map[string]*list.Element{}
	cacheLRU = list.New()
)

const (
	cacheTTL         = 30 * time.Minute
	cacheStaleTTL    = 6 * time.Hour
	cacheNegTTL      = 2 * time.Minute
	cacheSuggestTTL  = 5 * time.Minute
	cacheMax         = 256
	fetchTimeout     = 5 * time.Second
	requestTimeout   = 9 * time.Second
	defaultLimit     = 24
	maxLimit         = 40
	suggestLimit     = 6
	liveResultScore  = 82
	liveScoreFloor   = 28
	spellFixMinChars = 4
	spellFixScore    = 70
)

// cacheGet devuelve candidatos frescos. allowPartial acepta los que salieron de
// una búsqueda rápida (sugerencias), que pueden quedarse cortos.
func cacheGet(key string, allowPartial bool) ([]*candidate, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	el, ok := cacheMap[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*cacheItem)
	if time.Since(it.at) > it.ttl {
		return nil, false
	}
	if it.partial && !allowPartial {
		return nil, false
	}
	cacheLRU.MoveToFront(el)
	return it.cands, true
}

// cacheStale devuelve candidatos vencidos para no dejar al usuario sin respuesta
// cuando TMDB falla.
func cacheStale(key string) ([]*candidate, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	el, ok := cacheMap[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*cacheItem)
	if time.Since(it.at) > cacheStaleTTL {
		cacheLRU.Remove(el)
		delete(cacheMap, key)
		return nil, false
	}
	return it.cands, true
}

func cacheSet(key string, cands []*candidate, ttl time.Duration, partial bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if el, ok := cacheMap[key]; ok {
		it := el.Value.(*cacheItem)
		// una búsqueda completa siempre vale más que una rápida
		if partial && !it.partial && time.Since(it.at) <= it.ttl {
			return
		}
		it.cands, it.at, it.partial = cands, time.Now(), partial
		if !partial {
			it.ttl = ttl
		}
		cacheLRU.MoveToFront(el)
		return
	}
	if cacheLRU.Len() >= cacheMax {
		if back := cacheLRU.Back(); back != nil {
			cacheLRU.Remove(back)
			delete(cacheMap, back.Value.(*cacheItem).key)
		}
	}
	cacheMap[key] = cacheLRU.PushFront(&cacheItem{key: key, cands: cands, at: time.Now(), ttl: ttl, partial: partial})
}

func cacheFlush() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheMap = map[string]*list.Element{}
	cacheLRU = list.New()
}

// ---------- Una sola petición por consulta a la vez ----------

type call struct {
	done  chan struct{}
	cands []*candidate
	err   error
}

var (
	flightMu sync.Mutex
	flights  = map[string]*call{}
)

// fetchShared reutiliza el trabajo en curso de la misma consulta.
func fetchShared(ctx context.Context, key string, fetch func(context.Context) ([]*candidate, error)) ([]*candidate, error) {
	flightMu.Lock()
	if c, ok := flights[key]; ok {
		flightMu.Unlock()
		select {
		case <-c.done:
			return c.cands, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	flights[key] = c
	flightMu.Unlock()

	// El trabajo no depende del cliente que llegó primero: si se va, los demás
	// siguen esperando el mismo resultado.
	fctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	c.cands, c.err = fetch(fctx)
	cancel()
	close(c.done)

	flightMu.Lock()
	delete(flights, key)
	flightMu.Unlock()
	return c.cands, c.err
}

// ---------- Búsqueda ----------

func cacheKey(p parsed) string {
	k := p.Fold
	if p.Year != "" {
		k += "#" + p.Year
	}
	if p.From > 0 {
		k += "#" + strconv.Itoa(p.From) + "-" + strconv.Itoa(p.To)
	}
	return k
}

// candidates devuelve los candidatos de TMDB usando caché y peticiones agrupadas.
func candidates(ctx context.Context, p parsed, quick bool) ([]*candidate, error) {
	key := cacheKey(p)
	if c, ok := cacheGet(key, quick); ok {
		return c, nil
	}
	flightKey := key
	if quick {
		flightKey += "|q"
	}
	c, err := fetchShared(ctx, flightKey, func(ctx context.Context) ([]*candidate, error) {
		return fetchCandidates(ctx, p, quick)
	})
	if err == nil {
		ttl := cacheTTL
		if quick {
			ttl = cacheSuggestTTL
		}
		if len(c) == 0 {
			ttl = cacheNegTTL // no castigar a TMDB por búsquedas sin resultados
		}
		cacheSet(key, c, ttl, quick)
		return c, nil
	}
	if stale, ok := cacheStale(key); ok {
		return stale, nil
	}
	return nil, err
}

// liveResult arma el acceso directo a la transmisión.
func liveResult(q string) Result {
	return Result{
		Type: "live", Title: "Ver en vivo: " + q, Source: "YouTube", Via: "YouTube",
		URL:   "https://www.youtube.com/results?search_query=" + url.QueryEscape(q+" en vivo"),
		Score: liveResultScore, Match: 0,
	}
}

func runSearch(ctx context.Context, rawQuery string, limit int, quick bool) (*SearchResponse, error) {
	p := parseQuery(rawQuery)
	resp := &SearchResponse{Query: p.Query, Year: p.Year, Results: []Result{}}
	if !p.valid() {
		return resp, nil
	}

	cands, err := candidates(ctx, p, quick)
	if err != nil {
		return nil, err
	}
	results := rank(cands, p)

	if p.Live {
		// Búsqueda deportiva: si el catálogo no responde, solo el directo en vivo.
		if bestMatchText(results) < liveScoreFloor {
			results = nil
		}
		results = append(results, liveResult(p.Query))
		sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	} else if len(results) > 0 && results[0].Match < spellFixScore && len([]rune(p.Fold)) >= spellFixMinChars {
		// Coincidencia aproximada (errata o título parecido): propone el más cercano.
		resp.DidYouMean, resp.SpellFixed = results[0].Title, true
	}

	resp.Total = len(results)
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	resp.Results = results
	return resp, nil
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoKey):
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	case errors.Is(err, errRate):
		w.Header().Set("Retry-After", "20")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TMDB está recibiendo muchas consultas, inténtalo en un momento"})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "TMDB tardó demasiado en responder"})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "No se pudo consultar TMDB"})
	}
}

func cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func parseLimit(r *http.Request, def, max int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		return min(n, max)
	}
	return def
}

func handleSearch(quick bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cors(w, r) {
			return
		}
		def, max := defaultLimit, maxLimit
		ttl := 300
		if quick {
			def, max, ttl = suggestLimit, 12, 120
		}
		limit := parseLimit(r, def, max)

		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()

		resp, err := runSearch(ctx, r.URL.Query().Get("q"), limit, quick)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", "public, s-maxage="+strconv.Itoa(ttl)+", stale-while-revalidate=600")
		writeJSON(w, http.StatusOK, resp)
	}
}

func searchHandler(w http.ResponseWriter, r *http.Request) { handleSearch(false)(w, r) }

func suggestHandler(w http.ResponseWriter, r *http.Request) { handleSearch(true)(w, r) }

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

func titleHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	typ := r.URL.Query().Get("type")
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if (typ != "movie" && typ != "tv") || err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parámetros type e id inválidos"})
		return
	}
	country := strings.ToUpper(r.URL.Query().Get("country"))
	if !countryRe.MatchString(country) {
		country = strings.ToUpper(r.Header.Get("X-Vercel-IP-Country"))
	}
	if !countryRe.MatchString(country) {
		country = "CO"
	}
	serveCached(w, "title|"+typ+"|"+strconv.Itoa(id)+"|"+country, detailTTL, func(ctx context.Context) (any, error) {
		return fetchDetails(ctx, typ, id, country)
	})
}
