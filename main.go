package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type SearchResult struct {
	Title  string `json:"title"`
	Source string `json:"source"`
	URL    string `json:"url"`
	Type   string `json:"type"`
}

// Función principal de manejo HTTP
func searchHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		json.NewEncoder(w).Encode([]SearchResult{})
		return
	}

	results := performMetaSearch(query)
	json.NewEncoder(w).Encode(results)
}

// Ejecución multihilo (Goroutines) para consultar varias fuentes a la vez
func performMetaSearch(query string) []SearchResult {
	var wg sync.WaitGroup
	resultsChan := make(chan []SearchResult, 4)

	// 1. Rastreo de Películas y Series (Búsqueda directa en Cuevana)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resultsChan <- searchCuevana(query)
	}()

	// 2. Rastreo de Transmisiones de Deportes / Fútbol en Vivo
	wg.Add(1)
	go func() {
		defer wg.Done()
		resultsChan <- searchLiveSports(query)
	}()

	// 3. Consulta de Catálogo Global (TMDB / Cine)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resultsChan <- searchTMDB(query)
	}()

	// WaitGroup en goroutine para cerrar canal
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var finalResults []SearchResult
	for res := range resultsChan {
		finalResults = append(finalResults, res...)
	}

	return finalResults
}

// Fuente 1: Cuevana 3
func searchCuevana(query string) []SearchResult {
	encoded := url.QueryEscape(query)
	searchURL := fmt.Sprintf("https://cuevana3e.pro/?s=%s", encoded)

	return []SearchResult{
		{
			Title:  fmt.Sprintf("Ver '%s' en Cuevana 3", query),
			Source: "Cuevana HD",
			URL:    searchURL,
			Type:   "Película / Serie",
		},
	}
}

// Fuente 2: Deportes y Fútbol en Vivo
func searchLiveSports(query string) []SearchResult {
	encoded := url.QueryEscape(query)
	ytLiveURL := fmt.Sprintf("https://www.youtube.com/results?search_query=%s+en+vivo", encoded)
	splayerURL := fmt.Sprintf("https://splayer.lat/")

	return []SearchResult{
		{
			Title:  fmt.Sprintf("Transmisión en Vivo: %s", query),
			Source: "YouTube Live",
			URL:    ytLiveURL,
			Type:   "Fútbol / Deportes",
		},
		{
			Title:  fmt.Sprintf("Buscar '%s' en Servidores SPlayer", query),
			Source: "SPlayer Network",
			URL:    splayerURL,
			Type:   "Deportes / Stream",
		},
	}
}

// Fuente 3: Base de Datos de Cine (TMDB API)
func searchTMDB(query string) []SearchResult {
	apiKey := "15d2cfe02701e71b5006d0341cd00a3b"
	reqURL := fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&language=es-ES&query=%s&page=1", apiKey, url.QueryEscape(query))

	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var tmdbData struct {
		Results []struct {
			Title    string `json:"title"`
			Name     string `json:"name"`
			MediaType string `json:"media_type"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tmdbData); err != nil {
		return nil
	}

	var results []SearchResult
	for i, item := range tmdbData.Results {
		if i >= 3 {
			break
		}
		name := item.Title
		if name == "" {
			name = item.Name
		}
		if name == "" {
			continue
		}

		results = append(results, SearchResult{
			Title:  fmt.Sprintf("Ficha HD: %s", name),
			Source: "TheMovieDB",
			URL:    fmt.Sprintf("https://cuevana3e.pro/?s=%s", url.QueryEscape(name)),
			Type:   strings.Title(item.MediaType),
		})
	}

	return results
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/api/search", searchHandler)
	http.HandleFunc("/", searchHandler)
	http.ListenAndServe(":"+port, nil)
}
