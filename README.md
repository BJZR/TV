# TV
Página web para los tv :)

## Buscador
- `GET /api/search?q=batman 1989&limit=24` → resultados de TMDB (películas, series y obras de un actor), ordenados por relevancia (tolera acentos y erratas, entiende el año, usa popularidad).
- `GET /api/title?type=movie&id=123` → detalle, tráiler y plataformas donde verla legalmente en tu país.

## Configuración
Define la variable de entorno `TMDB_API_KEY` (en Vercel: Settings → Environment Variables).

Local: `TMDB_API_KEY=tu_clave go run .` y abre http://localhost:8080

Tests: `go test ./...`
