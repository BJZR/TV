package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestFoldAndParse(t *testing.T) {
	if got := fold("  El Señor de los Anillos: ¡La Comunidad! "); got != "el senor de los anillos la comunidad" {
		t.Fatalf("fold = %q", got)
	}
	q, y := parseQuery("batman (1989)")
	if q != "batman" || y != "1989" {
		t.Fatalf("parse = %q %q", q, y)
	}
	if q, y = parseQuery("2012"); q != "2012" || y != "" {
		t.Fatalf("parse solo año = %q %q", q, y)
	}
}

func TestTextScoreOrdering(t *testing.T) {
	exact := textScore("batman", "batman")
	prefix := textScore("batman", "batman el caballero de la noche")
	typo := textScore("avengrs", "avengers")
	other := textScore("batman", "superman")
	if !(exact > prefix && prefix > other) {
		t.Fatalf("orden inesperado: %v %v %v", exact, prefix, other)
	}
	if typo < 55 {
		t.Fatalf("la errata debería puntuar alto, got %v", typo)
	}
	if textScore("el padrino", "el padrino parte ii") <= textScore("padrino", "el padrino parte ii")-1 {
		// solo verifica que no explote
	}
}

func mockTMDB(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/search/multi":
			q := strings.ToLower(r.URL.Query().Get("query"))
			lang := r.URL.Query().Get("language")
			switch {
			case strings.Contains(q, "batman"):
				title := "Batman"
				if lang == "en-US" {
					title = "Batman"
				}
				json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
					{"id": 2, "media_type": "movie", "title": "Batman Forever", "original_title": "Batman Forever", "release_date": "1995-06-16", "popularity": 30.0, "vote_count": 3000, "poster_path": "/b.jpg", "overview": "x"},
					{"id": 1, "media_type": "movie", "title": title, "original_title": "Batman", "release_date": "1989-06-23", "popularity": 50.0, "vote_count": 8000, "vote_average": 7.2, "poster_path": "/a.jpg", "overview": "Un caballero oscuro"},
					{"id": 9, "media_type": "person", "name": "Persona", "known_for": []map[string]any{}},
				}})
			case strings.Contains(q, "tom hanks"):
				json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
					{"id": 5, "media_type": "person", "name": "Tom Hanks", "known_for": []map[string]any{
						{"id": 13, "media_type": "movie", "title": "Forrest Gump", "popularity": 80.0, "vote_count": 20000, "poster_path": "/f.jpg", "overview": "x", "release_date": "1994-07-06"},
					}},
				}})
			default:
				json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
			}
		case strings.HasPrefix(r.URL.Path, "/movie/"):
			json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "title": "Batman", "release_date": "1989-06-23", "runtime": 126,
				"genres": []map[string]any{{"name": "Fantasía"}},
				"videos": map[string]any{"results": []map[string]any{
					{"key": "t1", "site": "YouTube", "type": "Teaser"},
					{"key": "t2", "site": "YouTube", "type": "Trailer", "official": true},
				}},
				"watch/providers": map[string]any{"results": map[string]any{
					"CO": map[string]any{"link": "https://tmdb/watch", "flatrate": []map[string]any{{"provider_name": "Max", "logo_path": "/m.jpg"}}},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSearchEndToEnd(t *testing.T) {
	srv := mockTMDB(t)
	defer srv.Close()
	tmdbBase = srv.URL
	os.Setenv("TMDB_API_KEY", "test")

	resp, err := runSearch(context.Background(), "batman", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Results[0].ID != 1 {
		t.Fatalf("ranking inesperado: %+v", resp.Results)
	}

	resp, err = runSearch(context.Background(), "batman 1995", 10)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Year != "1995" || resp.Results[0].ID != 2 {
		t.Fatalf("el año debería priorizar Batman Forever: %+v", resp.Results)
	}

	resp, err = runSearch(context.Background(), "tom hanks", 10)
	if err != nil || len(resp.Results) != 1 || resp.Results[0].Via != "Tom Hanks" {
		t.Fatalf("known_for: %v %+v", err, resp)
	}

	resp, err = runSearch(context.Background(), "real madrid vs barcelona", 10)
	if err != nil || len(resp.Results) != 1 || resp.Results[0].Type != "live" {
		t.Fatalf("deportes: %v %+v", err, resp)
	}
}

func TestHandlers(t *testing.T) {
	srv := mockTMDB(t)
	defer srv.Close()
	tmdbBase = srv.URL
	os.Setenv("TMDB_API_KEY", "test")

	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=batman&limit=1", nil))
	var sr SearchResponse
	json.Unmarshal(rec.Body.Bytes(), &sr)
	if rec.Code != 200 || len(sr.Results) != 1 || sr.Total != 2 {
		t.Fatalf("search handler: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	titleHandler(rec, httptest.NewRequest("GET", "/api/title?type=movie&id=1&country=CO", nil))
	var d Details
	json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != 200 || d.Trailer != "https://www.youtube.com/watch?v=t2" || len(d.Providers) != 1 || d.Providers[0].Kind != "flatrate" {
		t.Fatalf("title handler: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	titleHandler(rec, httptest.NewRequest("GET", "/api/title?type=person&id=1", nil))
	if rec.Code != 400 {
		t.Fatalf("esperaba 400, got %d", rec.Code)
	}

	os.Unsetenv("TMDB_API_KEY")
	cacheMu.Lock()
	cacheMap = map[string]cacheEntry{}
	cacheMu.Unlock()
	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=batman", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "TMDB_API_KEY") {
		t.Fatalf("sin clave: %d %s", rec.Code, rec.Body.String())
	}
}
