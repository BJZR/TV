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

//go:embed index.html catalog.html
var site embed.FS

// La versión y la hora de arranque se ven en el log y en el pie de la página:
// así se sabe de un vistazo si el navegador está viendo esta build y no una
// anterior. La hora cambia en cada arranque; si no cambia, el servidor que
// está sirviendo la página no se reinició.
var version = "0.10.0"
var buildStamp = time.Now().UTC().Format("2006-01-02 15:04:05")

func main() {

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", searchHandler)
	mux.HandleFunc("/api/suggest", suggestHandler)
	mux.HandleFunc("/api/version", versionHandler)
	mux.HandleFunc("/resultados", pageHandler("/resultados", "catalog.html"))
	mux.HandleFunc("/resultados/", pageHandler("/resultados", "catalog.html"))
	mux.HandleFunc("/", homeHandler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           compress(mux),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("TV v%s escuchando en :%s (arrancado %s UTC)", version, port, buildStamp)
	log.Fatal(srv.ListenAndServe())
}

// versionHandler dice qué build está sirviendo. La página la consulta al
// cargarse y la muestra en el pie: si no coincide con lo que esperabas,
// recargar es tan simple como F5.
func versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"version": version, "built": buildStamp + " UTC",
	})
}

// homeHandler sirve la página (desde el binario, sin depender del disco) y
// responde rápido a las llamadas de verificación de Vercel.
func homeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	servePage(w, r, "index.html")
}

// pageHandler sirve una página embebida bajo /<path> y /<path>/.
func pageHandler(path, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path && r.URL.Path != path+"/" {
			http.NotFound(w, r)
			return
		}
		servePage(w, r, name)
	}
}

func servePage(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := site.ReadFile(name)
	if err != nil {
		http.Error(w, "no se encontró la página", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(body)))
}

// compress reduce el tamaño de las respuestas JSON.
func compress(next http.Handler) http.Handler {
	pool := sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
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
