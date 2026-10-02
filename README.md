# TV
Buscador de películas: escribes el nombre y te dice dónde verlas en toda la web.

No usa base de datos de películas ni claves de API: consulta un buscador web
(DuckDuckGo), clasifica en qué plataforma está cada resultado y ordena por
relevancia. Sin TMDB, sin registro, sin límites de cuota.

## Páginas
- `/` — launcher: caja de búsqueda con sugerencias y los canales de siempre.
- `/resultados?q=matrix` — resultados de la búsqueda.

## API
- `GET /api/search?q=matrix&limit=24` → páginas donde aparece la película,
  ordenadas. Cada resultado trae `title`, `url`, `host`, `snippet`, `year`,
  `platform`, `kind` (`plataforma`, `video`, `info`, `web`), `favicon` y `score`.
- `GET /api/suggest?q=mat` → lo que la gente suele escribir a partir de eso.

## Cómo decide el orden
1. Limpia la consulta: saca año, rango de años, palabras de ruido ("ver", "en
   hd", "latino"), pistas de tipo ("serie dark") e intención deportiva.
2. Busca el título limpio y las variantes "título + año" y "título ver en línea".
3. Puntúa cada página por coincidencia de texto con el título (tolera acentos,
   erratas y palabras de más), año, si es una plataforma de streaming y si el
   resumen promete película. Descarta lo que no habla de lo que buscabas.
4. Fusiona y deduplica: la misma página en dos idiomas o países es una sola.

## Caché
LRU en memoria: 30 min por consulta, 6 h de respaldo si el buscador falla y
varias personas Asking lo mismo al mismo tiempo comparten una sola petición.

## Arrancar
```bash
go run .          # http://localhost:8080
PORT=3000 go run .
go test ./...     # los tests no salen a internet
```

## Archivos
| Archivo | De qué se ocupa |
| --- | --- |
| `main.go` | servidor, rutas, gzip y páginas embebidas |
| `websearch.go` | consulta y lee el HTML del buscador, clasifica plataformas, ordena |
| `search.go` | caché, peticiones agrupadas y handlers |
| `query.go` | entiende lo que escribe la gente |
| `rank.go` | compara textos: acentos, erratas, títulos parecidos |
| `index.html` | launcher con el buscador |
| `catalog.html` | página de resultados |