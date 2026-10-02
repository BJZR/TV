package main

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result es una página de la web donde aparece la película: el buscador la
// devuelve tal cual, con su plataforma y su resumen para que se entienda de
// qué se trata sin abrir el enlace.
type Result struct {
	Title    string  `json:"title"`
	URL      string  `json:"url"`
	Host     string  `json:"host"`
	Snippet  string  `json:"snippet,omitempty"`
	Year     string  `json:"year,omitempty"`
	Platform string  `json:"platform,omitempty"`
	Kind     string  `json:"kind"` // web | plataforma | video | info
	Favicon  string  `json:"favicon,omitempty"`
	Source   string  `json:"source"`
	Score    float64 `json:"score"`
	Match    float64 `json:"-"`
}

type SearchResponse struct {
	Query      string   `json:"query"`
	Total      int      `json:"total"`
	Results    []Result `json:"results"`
	Suggestion string   `json:"suggestion,omitempty"`
	Engine     string   `json:"engine,omitempty"`
}

// ---------- Caché LRU con TTL y respaldo de datos viejos ----------

type cacheItem struct {
	key     string
	rs      []Result
	at      time.Time
	ttl     time.Duration
	partial bool // solo sugerencias: vale menos
}

var (
	cacheMu  sync.Mutex
	cacheMap = map[string]*list.Element{}
	cacheLRU = list.New()
)

const (
	cacheTTL      = 30 * time.Minute
	cacheStaleTTL = 6 * time.Hour
	cacheNegTTL   = 2 * time.Minute
	cacheSugTTL   = 10 * time.Minute
	cacheMax      = 256
	requestTimout = 12 * time.Second
	defaultLimit  = 24
	maxLimit      = 40
	suggestLimit  = 6
)

// cacheGet devuelve resultados frescos. allowPartial acepta los que salieron
// de una búsqueda rápida (sugerencias), que pueden quedarse cortos.
func cacheGet(key string, allowPartial bool) ([]Result, bool) {
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
	return it.rs, true
}

// cacheStale devuelve resultados vencidos para no dejar al usuario sin
// respuesta cuando el buscador falla.
func cacheStale(key string) ([]Result, bool) {
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
	return it.rs, true
}

func cacheSet(key string, rs []Result, ttl time.Duration, partial bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if el, ok := cacheMap[key]; ok {
		it := el.Value.(*cacheItem)
		// una búsqueda completa siempre vale más que una rápida
		if partial && !it.partial && time.Since(it.at) <= it.ttl {
			return
		}
		it.rs, it.at, it.partial = rs, time.Now(), partial
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
	cacheMap[key] = cacheLRU.PushFront(&cacheItem{key: key, rs: rs, at: time.Now(), ttl: ttl, partial: partial})
}

func cacheFlush() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheMap = map[string]*list.Element{}
	cacheLRU = list.New()
}

// ---------- Una sola petición por consulta a la vez ----------

type call struct {
	done chan struct{}
	rs   []Result
	err  error
}

var (
	flightMu sync.Mutex
	flights  = map[string]*call{}
)

// fetchShared reutiliza el trabajo en curso de la misma consulta.
func fetchShared(ctx context.Context, key string, fetch func(context.Context) ([]Result, error)) ([]Result, error) {
	flightMu.Lock()
	if c, ok := flights[key]; ok {
		flightMu.Unlock()
		select {
		case <-c.done:
			return c.rs, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	flights[key] = c
	flightMu.Unlock()

	// El trabajo no depende del cliente que llegó primero: si se va, los demás
	// siguen esperando el mismo resultado.
	fctx, cancel := context.WithTimeout(context.Background(), requestTimout)
	c.rs, c.err = fetch(fctx)
	cancel()
	close(c.done)

	flightMu.Lock()
	delete(flights, key)
	flightMu.Unlock()
	return c.rs, c.err
}

// ---------- Búsqueda ----------

func cacheKey(p parsed) string {
	k := p.Fold
	if p.Year != "" {
		k += "#" + p.Year
	}
	return k
}

// results busca en la web con caché y peticiones agrupadas.
func results(ctx context.Context, p parsed) ([]Result, error) {
	key := cacheKey(p)
	if rs, ok := cacheGet(key, true); ok {
		return rs, nil
	}
	rs, err := fetchShared(ctx, key, func(ctx context.Context) ([]Result, error) {
		return searchWeb(ctx, p)
	})
	if err == nil {
		ttl := cacheTTL
		if len(rs) == 0 {
			ttl = cacheNegTTL // no castigar al buscador por consultas sin resultados
		}
		cacheSet(key, rs, ttl, false)
		return rs, nil
	}
	if stale, ok := cacheStale(key); ok {
		return stale, nil
	}
	return nil, err
}

func runSearch(ctx context.Context, rawQuery string, limit int) (*SearchResponse, error) {
	p := parseQuery(rawQuery)
	resp := &SearchResponse{Query: p.Query, Engine: "DuckDuckGo", Results: []Result{}}
	if !p.valid() {
		return resp, nil
	}

	rs, err := results(ctx, p)
	if err != nil {
		return nil, err
	}
	ranked := rankResults(rs, p)

	resp.Total = len(ranked)
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	resp.Results = ranked
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
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "El buscador tardó demasiado en responder"})
	case errors.Is(err, errNoEngine):
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "No se pudo consultar el buscador"})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "No se pudo consultar el buscador"})
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

func searchHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	limit := parseLimit(r, defaultLimit, maxLimit)

	ctx, cancel := context.WithTimeout(r.Context(), requestTimout)
	defer cancel()

	resp, err := runSearch(ctx, r.URL.Query().Get("q"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "public, s-maxage=300, stale-while-revalidate=600")
	writeJSON(w, http.StatusOK, resp)
}

// suggestHandler devuelve lo que la gente suele escribir a partir de lo tecleado.
func suggestHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"query": q, "results": []string{}})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimout)
	defer cancel()

	out, err := suggestWeb(ctx, q)
	if err != nil && len(out) == 0 {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "public, s-maxage=600")
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "results": out})
}
