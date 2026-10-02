package main

import (
	"compress/gzip"
	"embed"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var site embed.FS

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", searchHandler)
	mux.HandleFunc("/api/suggest", suggestHandler)
	mux.HandleFunc("/api/title", titleHandler)
	mux.HandleFunc("/", homeHandler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           compress(mux),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("TV escuchando en :%s", port)
	log.Fatal(srv.ListenAndServe())
}

// homeHandler sirve la página (desde el binario, sin depender del disco) y
// responde rápido a las llamadas de verificación de Vercel.
func homeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := site.ReadFile("index.html")
	if err != nil {
		http.Error(w, "no se encontró la página", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(body)))
}

// compress reduce el tamaño de las respuestas JSON.
func compress(next http.Handler) http.Handler {
	pool := sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		zw := pool.Get().(*gzip.Writer)
		zw.Reset(w)
		defer func() {
			zw.Close()
			pool.Put(zw)
		}()
		next.ServeHTTP(&gzipWriter{ResponseWriter: w, Writer: zw}, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	io.Writer
}

func (g *gzipWriter) Write(p []byte) (int, error) { return g.Writer.Write(p) }

// WriteHeader descarta el Content-Length original: ya no corresponde al cuerpo
// comprimido.
func (g *gzipWriter) WriteHeader(code int) {
	g.ResponseWriter.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}
