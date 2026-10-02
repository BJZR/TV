package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// fold normaliza texto: minúsculas, sin acentos ni signos, espacios simples.
func fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'á', 'à', 'ä', 'â', 'ã', 'å':
			r = 'a'
		case 'é', 'è', 'ë', 'ê':
			r = 'e'
		case 'í', 'ì', 'ï', 'î':
			r = 'i'
		case 'ó', 'ò', 'ö', 'ô', 'õ':
			r = 'o'
		case 'ú', 'ù', 'ü', 'û':
			r = 'u'
		case 'ñ':
			r = 'n'
		case 'ç':
			r = 'c'
		case 'ý', 'ÿ':
			r = 'y'
		case 'ø':
			r = 'o'
		case 'æ':
			r = 'a'
		case 'œ':
			r = 'o'
		case 'ł':
			r = 'l'
		case 'đ':
			r = 'd'
		case 'ş':
			r = 's'
		case 'ğ':
			r = 'g'
		case 'ı':
			r = 'i'
		case '\'', '’', '`', '´':
			continue // "marvel's" -> "marvels"
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevSpace = false
		} else if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// contentTokens parte un texto ya normalizado y quita palabras vacías.
func contentTokens(s string) []string {
	f := strings.Fields(s)
	out := make([]string, 0, len(f))
	for _, w := range f {
		if !isStopWord(w) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return f
	}
	return out
}

// editDistance distancia de edición con transposiciones (Damerau OSA).
func editDistance(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			best := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				best = min(best, prev2[j-2]+1) // letras cambiadas de orden
			}
			cur[j] = best
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(b)]
}

// similarity devuelve 0..1; tolera erratas solo en palabras de 4+ letras.
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 4 || len(rb) < 4 {
		return 0
	}
	d := editDistance(ra, rb)
	sim := 1 - float64(d)/float64(max(len(ra), len(rb)))
	if min(len(ra), len(rb)) <= 4 && d > 1 {
		sim = math.Min(sim, 0.55) // "gato" y "pato" no son la misma palabra
	}
	return math.Max(sim, 0)
}

// bestToken puntúa cuánto se parece una palabra de la consulta a una del título.
func bestToken(a string, tt []string) float64 {
	best := 0.0
	for _, b := range tt {
		var s float64
		switch {
		case a == b:
			s = 1
		case len(a) >= 2 && strings.HasPrefix(b, a):
			s = 0.9 - math.Min(0.25, 0.03*float64(len([]rune(b))-len([]rune(a))))
		default:
			if sim := similarity(a, b); sim >= 0.6 {
				s = sim * 0.92
			}
		}
		best = math.Max(best, s)
	}
	return best
}

// textScore puntúa (0..100) qué tan bien un título (ya normalizado) responde a
// la consulta (normalizada): cobertura de palabras, frase completa, prefijo,
// subsecuencia y penalización por palabras de sobra.
func textScore(q, t string) float64 {
	if q == "" || t == "" {
		return 0
	}
	if q == t {
		return 100
	}
	qt, tt := contentTokens(q), contentTokens(t)
	if len(qt) == 0 || len(tt) == 0 {
		return 0
	}
	// Solo cambiaban los artículos: "the matrix" es "matrix".
	if strings.Join(qt, " ") == strings.Join(tt, " ") {
		return 100
	}
	var sum float64
	for _, a := range qt {
		sum += bestToken(a, tt)
	}
	score := 78 * sum / float64(len(qt))

	// La consulta completa aparece dentro del título: "blade runner" ~ "blade runner 2049".
	if strings.Contains(t, q) {
		score = math.Max(score, 74)
	}
	// El título empieza con la consulta: "batman" ~ "batman begins".
	if strings.HasPrefix(t, q) {
		extra := len(tt) - len(qt)
		score = math.Max(score, 86-math.Min(22, float64(extra)*4))
	}
	if score < 74 && isSubsequence(q, t) {
		score = math.Max(score, 62)
	}
	if extra := len(tt) - len(qt); extra > 0 {
		score -= math.Min(14, float64(extra)*2.5)
	}
	return math.Max(0, math.Min(100, score))
}

// isSubsequence indica si q aparece en t respetando el orden.
func isSubsequence(q, t string) bool {
	if len(q) < 3 {
		return false
	}
	qr := []rune(q)
	i := 0
	for _, r := range t {
		if r == qr[i] {
			i++
			if i == len(qr) {
				return true
			}
		}
	}
	return false
}

func bestText(q string, titles ...string) float64 {
	best := 0.0
	for _, t := range titles {
		best = math.Max(best, textScore(q, fold(t)))
	}
	return best
}

func yearOf(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

func trimRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// matchScore puntúa un candidato contra la consulta entendida (0..100).
func matchScore(c *candidate, p parsed) float64 {
	text := bestText(p.Fold, c.Title, c.OrigTitle, c.EnTitle)
	if p.RawFold != p.Fold {
		text = math.Max(text, bestText(p.RawFold, c.Title, c.OrigTitle, c.EnTitle))
	}

	// Actor/director: la consulta era su nombre, no el título de la obra.
	if c.Via != "" {
		via := textScore(p.Fold, fold(c.Via))
		if text >= 45 && via >= 60 {
			text += 4 // el título también coincide
		}
		text = math.Max(text, via*0.62)
	}

	if p.Kind != "" {
		if p.Kind == c.Type {
			text += 6
		} else {
			text -= 4
		}
	}

	switch {
	case p.From > 0:
		cy, _ := strconv.Atoi(yearOf(c.Date))
		if cy >= p.From && cy <= p.To {
			text += 16
		} else if cy > 0 {
			text -= 8
		}
	case p.Year != "":
		want, _ := strconv.Atoi(p.Year)
		cy, _ := strconv.Atoi(yearOf(c.Date))
		switch d := abs(cy - want); {
		case cy == 0:
		case d == 0:
			text += 18
		case d == 1:
			text += 7
		default:
			text -= 6
		}
	}
	return math.Max(0, math.Min(100, text))
}

// ratingBonus usa la media ponderada de IMDb: una nota alta con pocos votos no
// sube tanto como una nota alta con muchos votos.
func ratingBonus(c *candidate) float64 {
	if c.Votes <= 0 || c.Rating <= 0 {
		return 0
	}
	const m, global = 1500.0, 6.6
	v := float64(c.Votes)
	weighted := (v/(v+m))*c.Rating + (m/(v+m))*global
	return math.Max(-9, math.Min(9, (weighted-global)*3))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// rank puntúa y ordena candidatos combinando coincidencia de texto, año,
// popularidad y calidad de la votación.
func rank(cands []*candidate, p parsed) []Result {
	type scored struct {
		r     Result
		text  float64
		score float64
		pop   float64
	}
	all := make([]scored, 0, len(cands))
	for _, c := range cands {
		if c == nil {
			continue
		}
		text := matchScore(c, p)
		score := text
		score += math.Min(12, 4.5*math.Log10(1+c.Pop))
		score += ratingBonus(c)
		if c.Poster == "" {
			score -= 5
		}
		if c.Overview == "" && c.Votes < 10 {
			score -= 6
		}
		if c.Votes == 0 && c.Pop < 1 {
			score -= 4
		}
		all = append(all, scored{
			r: Result{
				ID: c.ID, Type: c.Type, Title: c.Title, OriginalTitle: c.OrigTitle,
				Year: yearOf(c.Date), Overview: trimRunes(c.Overview, 220), Poster: c.Poster,
				Backdrop: c.Backdrop, Rating: math.Round(c.Rating*10) / 10, Votes: c.Votes,
				Source: "TMDB", Via: c.Via,
				Match: text, Score: math.Round(score*10) / 10,
			},
			text: text, score: score, pop: c.Pop,
		})
	}

	// Si hay buenas coincidencias, descarta el ruido de TMDB.
	strong := 0
	for _, s := range all {
		if s.text >= 55 {
			strong++
		}
	}
	cut := 0.0
	switch {
	case strong >= 3:
		cut = 30
	case strong >= 2:
		cut = 26
	case strong == 1:
		cut = 12
	}
	out := make([]scored, 0, len(all))
	for _, s := range all {
		if cut > 0 && s.text < cut {
			continue
		}
		out = append(out, s)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if out[i].text != out[j].text {
			return out[i].text > out[j].text
		}
		return out[i].pop > out[j].pop
	})

	res := make([]Result, len(out))
	for i, s := range out {
		res[i] = s.r
	}
	return res
}

// bestMatchText devuelve la mejor coincidencia de texto de los resultados.
func bestMatchText(results []Result) float64 {
	best := 0.0
	for _, r := range results {
		best = math.Max(best, r.Match)
	}
	return best
}
