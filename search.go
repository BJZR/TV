package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
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
}

type SearchResponse struct {
	Query   string   `json:"query"`
	Year    string   `json:"year,omitempty"`
	Total   int      `json:"total"`
	Results []Result `json:"results"`
}

// ---------- Caché en memoria con TTL ----------

type cacheEntry struct {
	at    time.Time
	cands []*candidate
}

var (
	cacheMu  sync.RWMutex
	cacheMap = map[string]cacheEntry{}
)

const (
	cacheTTL = 15 * time.Minute
	cacheMax = 500
)

func cacheGet(k string) ([]*candidate, bool) {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	e, ok := cacheMap[k]
	if !ok || time.Since(e.at) > cacheTTL {
		return nil, false
	}
	return e.cands, true
}

func cacheSet(k string, c []*candidate) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if len(cacheMap) >= cacheMax {
		for key, e := range cacheMap {
			if time.Since(e.at) > cacheTTL {
				delete(cacheMap, key)
			}
		}
		if len(cacheMap) >= cacheMax {
			cacheMap = map[string]cacheEntry{}
		}
	}
	cacheMap[k] = cacheEntry{time.Now(), c}
}

// ---------- Búsqueda ----------

func runSearch(ctx context.Context, rawQuery string, limit int) (*SearchResponse, error) {
	q, year := parseQuery(rawQuery)
	resp := &SearchResponse{Query: q, Year: year, Results: []Result{}}
	if fold(q) == "" {
		return resp, nil
	}
	rawFold, cleanFold := fold(rawQuery), fold(q)
	key := rawFold

	cands, ok := cacheGet(key)
	if !ok {
		queries := []string{q}
		if year != "" {
			queries = append(queries, strings.Join(strings.Fields(rawQuery), " ")) // "Blade Runner 2049"
		}
		var err error
		if cands, err = fetchCandidates(ctx, queries); err != nil {
			return nil, err
		}
		cacheSet(key, cands)
	}

	results := rank(cands, rawFold, cleanFold, year)
	if looksSporty(cleanFold) {
		results = append(results, Result{
			Type: "live", Title: "Transmisión en vivo: " + q, Source: "YouTube",
			URL:   "https://www.youtube.com/results?search_query=" + url.QueryEscape(q+" en vivo"),
			Score: 75,
		})
		// Reordena el resultado en vivo según su puntaje.
		for i := len(results) - 1; i > 0 && results[i].Score > results[i-1].Score; i-- {
			results[i], results[i-1] = results[i-1], results[i]
		}
	}
	resp.Total = len(results)
	if len(results) > limit {
		results = results[:limit]
	}
	resp.Results = results
	return resp, nil
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoKey):
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

func searchHandler(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	limit := 24
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 40)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	resp, err := runSearch(ctx, r.URL.Query().Get("q"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "public, s-maxage=300, stale-while-revalidate=600")
	writeJSON(w, http.StatusOK, resp)
}

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
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	d, err := fetchDetails(ctx, typ, id, country)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "public, s-maxage=3600, stale-while-revalidate=86400")
	writeJSON(w, http.StatusOK, d)
}
