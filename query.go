package main

import (
	"regexp"
	"strconv"
	"strings"
)

// Palabras vacías: artículos y preposiciones que aparecen o desaparecen según
// el idioma del catálogo ("The Matrix" / "Matrix"). No aportan a la coincidencia.
var stopWords = map[string]bool{
	"el": true, "la": true, "los": true, "las": true, "un": true, "una": true,
	"unos": true, "unas": true, "lo": true, "al": true, "le": true, "les": true,
	"the": true, "a": true, "an": true, "of": true, "and": true, "with": true,
	"y": true, "e": true, "de": true, "del": true, "du": true, "des": true,
	"au": true, "aux": true, "il": true, "os": true, "as": true, "on": true,
	"in": true, "der": true, "die": true, "das": true,
}

func isStopWord(w string) bool { return stopWords[w] }

// typeHints: la gente escribe "serie batman" o "pelicula matrix" para desambiguar.
var typeHints = map[string]string{
	"serie": "tv", "series": "tv", "serial": "tv",
	"pelicula": "movie", "peliculas": "movie", "movie": "movie", "film": "movie", "films": "movie",
}

// noiseWords: palabras de la web que nunca están en el título de TMDB.
var noiseWords = map[string]bool{
	"ver": true, "viendo": true, "watch": true, "online": true, "gratis": true, "free": true,
	"hd": true, "fullhd": true, "1080p": true, "720p": true, "480p": true, "4k": true, "uhd": true,
	"blu": true, "ray": true, "bluray": true, "dvd": true, "rip": true, "audio": true,
	"subtitulada": true, "subtitulado": true, "subtitled": true, "dub": true, "doblaje": true,
	"espanol": true, "castellano": true, "latino": true, "ingles": true, "english": true,
	"completo": true, "completa": true, "pelicula": true, "peliculas": true, "movie": true, "film": true, "films": true,
	"temporada": true, "temporadas": true, "season": true, "seasons": true, "capitulo": true, "episodio": true,
	"trailer": true, "trailers": true, "resumen": true, "critica": true, "estreno": true,
	"para": true, "veronline": true, "nuevo": true, "nueva": true, "en": true,
}

var (
	yearRe      = regexp.MustCompile(`^(19\d{2}|20\d{2})$`)
	yearRangeRe = regexp.MustCompile(`\b((?:19|20)\d{2})\s*(?:-|–|—|a|hasta|to)\s*((?:19|20)\d{2})\b`)
	sportsRe    = regexp.MustCompile(`\b(vs|en vivo|en directo|directo|futbol|partido|liga|champions|mundial|copa|eliminatorias|premier|la liga|serie a|bundesliga|nba|nfl|ufc|f1|formula 1|tenis|boxeo|beisbol|libertadores|gol|partidos|arbitro|estadio|messi|ronaldo)\b`)
)

const (
	maxQueryRunes = 120
	minQueryRunes = 2
)

// parsed es la consulta ya entendida: texto limpio, año, pista de tipo e intención.
type parsed struct {
	Raw      string // texto tal como lo escribió el usuario
	Query    string // texto limpio que se envía a TMDB
	Fold     string // Query normalizada
	RawFold  string // Raw normalizada
	Year     string // año suelto, p.ej. "1989"
	From, To int    // rango de años, p.ej. 2000-2009
	Kind     string // "movie" | "tv" | ""
	Live     bool   // parece una búsqueda deportiva
	Tokens   []string
}

// parseQuery limpia la consulta: saca año, rango de años, palabras de ruido,
// deja la pista de tipo y marca la intención deportiva.
func parseQuery(raw string) parsed {
	p := parsed{Raw: raw}
	s := strings.Join(strings.Fields(raw), " ")
	if r := []rune(s); len(r) > maxQueryRunes {
		s = string(r[:maxQueryRunes])
	}
	p.RawFold = fold(s)

	// Rango de años en cualquier posición: "star wars 1977-1983".
	if m := yearRangeRe.FindStringSubmatch(s); m != nil {
		p.From, _ = strconv.Atoi(m[1])
		p.To, _ = strconv.Atoi(m[2])
		if p.From > p.To {
			p.From, p.To = p.To, p.From
		}
		s = yearRangeRe.ReplaceAllString(s, " ")
	}

	fields := strings.Fields(s)

	// Primero se limpian palabras de ruido y pistas de tipo: la gente escribe
	// "ver batman 1989 en hd" y el año está en medio.
	var out []string
	for _, f := range fields {
		lf := fold(f)
		if lf == "" {
			continue
		}
		if p.Kind == "" {
			if k, ok := typeHints[lf]; ok {
				p.Kind = k
				continue
			}
		}
		if noiseWords[lf] {
			continue
		}
		out = append(out, f)
	}
	clean := out
	if len(clean) == 0 {
		clean = fields // todo era ruido: mejor devolver algo que nada
	}

	// Año suelto al final: "batman 1989", "batman (1989)", "blade runner, 2049".
	if p.From == 0 && len(clean) > 1 {
		last := strings.Trim(clean[len(clean)-1], "()[]{}.,;:!?-")
		if yearRe.MatchString(last) {
			p.Year = last
			clean = clean[:len(clean)-1]
		}
	}

	p.Query = strings.Join(clean, " ")
	p.Fold = fold(p.Query)
	p.Tokens = strings.Fields(p.Fold)
	p.Live = sportsRe.MatchString(p.Fold)
	return p
}

// valid indica si hay algo consultable.
func (p parsed) valid() bool {
	n := len([]rune(p.Fold))
	return n >= minQueryRunes || (n == 1 && p.Kind != "")
}

// primary devuelve las consultas que siempre se prueban (incluye el año, que
// TMDB sí usa para títulos numéricos como "Blade Runner 2049").
func (p parsed) primary() []string {
	out := []string{p.Query}
	if p.Year != "" && p.Query != "" {
		out = append(out, p.Query+" "+p.Year)
	}
	return out
}

// variants son consultas alternativas para cuando la primera no encuentra nada:
// erratas, búsquedas a medio escribir y títulos con palabras de sobra.
func (p parsed) variants() []string {
	var out []string
	seen := map[string]bool{}
	for _, q := range p.primary() {
		seen[strings.TrimSpace(q)] = true
	}
	add := func(s string) {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" || seen[s] || len([]rune(fold(s))) < minQueryRunes {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	words := strings.Fields(p.Query)

	// Sin palabras vacías: "the godfather" -> "godfather".
	if v := strings.Join(dropStopWords(p.Tokens), " "); v != "" {
		add(v)
	}

	// Búsqueda a medio escribir: "harry potter y la" -> "harry potter".
	if len(words) > 1 {
		last := fold(words[len(words)-1])
		if len([]rune(last)) <= 3 || isStopWord(last) {
			add(strings.Join(words[:len(words)-1], " "))
		}
	}

	// Prefijos: toleran erratas ("avengrs" -> "aven") y búsquedas parciales.
	n := 5
	if len(p.Tokens) == 1 {
		n = 4
	}
	add(truncateTokens(p.Tokens, n))

	// Solo la palabra más larga: último recurso cuando nada cuadra.
	if len(p.Tokens) > 1 {
		longest := p.Tokens[0]
		for _, t := range p.Tokens {
			if len(t) > len(longest) {
				longest = t
			}
		}
		add(truncateTokens([]string{longest}, min(6, len(longest))))
	}
	return out
}

func dropStopWords(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if !isStopWord(t) {
			out = append(out, t)
		}
	}
	return out
}

// truncateTokens recorta palabras largas para buscar por prefijo.
func truncateTokens(tokens []string, n int) string {
	out := make([]string, 0, len(tokens))
	changed := false
	for _, t := range tokens {
		r := []rune(t)
		if len(r) > n {
			t, changed = string(r[:n]), true
		}
		out = append(out, t)
	}
	if !changed {
		return ""
	}
	return strings.Join(out, " ")
}
