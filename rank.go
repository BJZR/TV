package main

import (
	"math"
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

func trimRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
