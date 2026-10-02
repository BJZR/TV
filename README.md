# TV
Buscador de películas: escribes el nombre y te dice dónde verlas en toda la web.

No usa base de datos de películas ni claves de API: consulta un buscador web
(DuckDuckGo), clasifica en qué plataforma está cada resultado y ordena por
relevancia. Sin TMDB, sin registro, sin límites de cuota.

## Páginas
- `/` — launcher: caja de búsqueda con sugerencias y los canales de siempre.
- `/resultados?q=matrix` — resultados de la búsqueda.

## API
- `GET /api/search?q=matrix&limit=24` → páginas donde aparece la película, ya
  ordenadas. Cada resultado trae `title`, `url`, `host`, `snippet`, `year`,
  `platform`, `kind` (`plataforma`, `video`, `info`, `web`), `favicon`, `source`,
  `votes` (cuántos índices lo encontraron) y `score`. La respuesta incluye
  `sources`: los índices que respondieron.
- `GET /api/suggest?q=mat` → lo que la gente escribe a partir de eso.

## Cómo busca en toda la web

Un solo buscador solo ve lo que él ha rastreado. Por eso se pregunta a varios
índices a la vez y se fusiona lo que devuelven: la unión llega a páginas que
ninguno habría encontrado solo.

| Índice | Qué aporta |
| --- | --- |
| **DuckDuckGo** | el grueso de la web, en dos variantes (html y lite) |
| **Wikipedia** | el nombre oficial, el año y los títulos alternativos |
| **Internet Archive** | películas de dominio público que se pueden ver ahí mismo |
| **Wiby** | un índice independiente: páginas personales y sitios olvidados |

Las fuentes que fallan, piden captcha o tardan se saltan y la búsqueda sigue con los
demás; la respuesta dice siempre en cuáles salió algo. Las sugerencias van
ainstead más lejos: se consulta con seis autocompletados (DuckDuckGo, Google,
Brave, Bing, Yahoo y Yandex) y se mezclan por acuerdo: la frase que proponen más
buscadores es la que más gente escribe.

Las sugerencias se filtran: se descarta lo que no empieza por lo tecleado y lo
que viene en alfabeto cirílico, que Yandex devuelve a menudo.

### Motores que no están

Mojeek, Brave, Ecosia, Startpage, Yandex, Bing y las instancias públicas de
SearXNG responden con captcha, challenge o 429 desde IPs de nube. No están en el
registro a propósito: cuando están bloqueados solo suman espera. Si desde tu
servidor alguno responde, se añade al registro (`webSources` en `sources.go`) y
suma cobertura sin tocar nada más.

## Cómo decide el orden
1. Limpia la consulta: saca año, rango de años, palabras de ruido ("ver", "en
   hd", "latino"), pistas de tipo ("serie dark") e intención deportiva.
2. Busca el título limpio y las variantes "título + año" y "título ver en línea".
3. Puntúa cada página por coincidencia de texto con el título (tolera acentos,
   erratas y palabras de más), año, si es una plataforma de streaming y si el
   resumen promete película. Descarta lo que no habla de lo que buscabas.
4. Fusiona y deduplica: la misma página en dos idiomas o países es una sola, y
   si varios índices la señalan sube en el orden.
5. Descarta lo que no es la película: si pediste un año, la que lo lleva en el
   título se queda fuera; y de una misma web no salen más de dos resultados, para
   que una plataforma no se lleve la pantalla entera.

## Caché
LRU en memoria: 30 min por consulta, 6 h de respaldo si el buscador falla y
varias personas Asking lo mismo al mismo tiempo comparten una sola petición.

## Arrancar
```bash
go run .          # http://localhost:8080
PORT=3000 go run .
go test ./...     # los tests no salen a internet
```

## Dónde va el dominio del backend
Las dos páginas leen el dominio de la API de **un solo sitio**, la etiqueta
`tv-api` de la cabecera (`index.html`, `catalog.html`):

```html
<meta name="tv-api" content="">
```

Vacío = el backend está en el mismo dominio que la página (lo normal). Si el
HTML se sirve desde un hosting y el backend vive en otro dominio, ahí se pone:

```html
<meta name="tv-api" content="https://tv-api.vercel.app">
```

Nada más: `fetch(API + '/api/search…')` ya usa esa variable. El backend envía
`Access-Control-Allow-Origin: *`, así que el navegador permite la llamada
cruzada sin configurar nada en el otro lado.

### Si el backend va en Vercel
Vercel no ejecuta un `go run`: necesita una función serverless, es decir un
`api/index.go` con `func Handler(w http.ResponseWriter, r *http.Request)`.
Además `//go:embed` no puede leer archivos de fuera de su carpeta, así que las
páginas tendrían que moverse dentro del paquete. Es un refactor, no un ajuste
de configuración.

Las dos salidas:

| | Qué hacer | Cuándo |
| --- | --- | --- |
| **A. Todo en un dominio** | Sirve el HTML y la API desde el mismo sitio (el propio Go, o el HTML como estático y `/api` con un proxy). `tv-api` vacío. | La más simple: sin CORS, sin dominios que sincronizar |
| **B. Dominios separados** | Rellena `tv-api` con la URL del backend. | Si el HTML está en un hosting estático y el backend en otro sitio |

## Archivos
| Archivo | De qué se ocupa |
| --- | --- |
| `main.go` | servidor, rutas, gzip y páginas embebidas |
| `sources.go` | los cuatro índices, las seis sugerencias y la fusión |
| `websearch.go` | lee el HTML, clasifica plataformas y ordena por puntuación |
| `search.go` | caché, peticiones agrupadas y handlers |
| `query.go` | entiende lo que escribe la gente |
| `rank.go` | compara textos: acentos, erratas, títulos parecidos |
| `index.html` | launcher con el buscador |
| `catalog.html` | página de resultados |