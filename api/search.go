package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type SearchResult struct {
	Title  string `json:"title"`
	Source string `json:"source"`
	URL    string `json:"url"`
	Type   string `json:"type"`
}

func Handler(w http.ResponseWriter, r *http.Request) {
	// Permitir CORS para la consulta desde la Smart TV
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	query := r.URL.Query().Get("q")
	if query == "" {
		json.NewEncoder(w).Encode([]SearchResult{})
		return
	}

	// Búsqueda global simulando las fuentes que rastrea un motor tipo Seekee
	encodedQuery := url.QueryEscape(query)

	results := []SearchResult{
		{
			Title:  fmt.Sprintf("Ver '%s' en Servidor Principal (HD)", query),
			Source: "Google Web Index",
			URL:    fmt.Sprintf("https://www.google.com/search?q=ver+%s+online+gratis+embed", encodedQuery),
			Type:   "Pelicula/Serie",
		},
		{
			Title:  fmt.Sprintf("Transmisión en Vivo: %s", query),
			Source: "Global Live Sports",
			URL:    fmt.Sprintf("https://www.youtube.com/results?search_query=%s+en+vivo", encodedQuery),
			Type:   "Deportes/Fútbol",
		},
		{
			Title:  fmt.Sprintf("Opciones de Reproducción para '%s'", query),
			Source: "DuckDuckGo Video Stream",
			URL:    fmt.Sprintf("https://duckduckgo.com/?q=%s+stream+online&iax=videos&ia=videos", encodedQuery),
			Type:   "Stream Web",
		},
	}

	json.NewEncoder(w).Encode(results)
}
