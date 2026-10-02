package main

import (
	"math"
	"regexp"
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

// parseQuery limpia la consulta y extrae un año final opcional ("batman 1989", "batman (1989)").
func parseQuery(raw string) (query, year string) {
	raw = strings.Join(strings.Fields(raw), " ")
	if r := []rune(raw); len(r) > 100 {
		raw = string(r[:100])
	}
	fields := strings.Fields(raw)
	if len(fields) > 1 {
		last := strings.Trim(fields[len(fields)-1], "()[]")
		if n, err := strconv.Atoi(last); err == nil && len(last) == 4 && n >= 1900 && n <= 2100 {
			return strings.Join(fields[:len(fields)-1], " "), last
		}
	}
	return raw, ""
}

func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
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
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
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
	l := max(len(ra), len(rb))
	return 1 - float64(levenshtein(ra, rb))/float64(l)
}

// textScore puntúa (0..100) qué tan bien un título (ya normalizado) responde a la consulta (normalizada).
func textScore(q, t string) float64 {
	if q == "" || t == "" {
		return 0
	}
	if q == t {
		return 100
	}
	if strings.HasPrefix(t, q) {
		return 88 - math.Min(10, float64(len(t)-len(q))*0.3)
	}
	qt, tt := strings.Fields(q), strings.Fields(t)
	var sum float64
	for _, a := range qt {
		best := 0.0
		for _, b := range tt {
			var s float64
			switch {
			case a == b:
				s = 1
			case len(a) >= 2 && strings.HasPrefix(b, a):
				s = 0.9
			default:
				if sim := similarity(a, b); sim >= 0.7 {
					s = sim * 0.9
				}
			}
			best = math.Max(best, s)
		}
		sum += best
	}
	score := 80 * sum / float64(len(qt))
	if strings.Contains(t, q) {
		score = math.Max(score, 70)
	}
	if extra := len(tt) - len(qt); extra > 0 {
		score -= math.Min(12, float64(extra)*2)
	}
	return math.Max(score, 0)
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

// rank puntúa y ordena candidatos combinando coincidencia de texto, año y popularidad.
func rank(cands []*candidate, rawFold, cleanFold, year string) []Result {
	type scored struct {
		r    Result
		text float64
		pop  float64
	}
	var all []scored
	for _, c := range cands {
		titles := []string{c.Title, c.OrigTitle, c.EnTitle}
		tClean := bestText(cleanFold, titles...)
		tFull := tClean
		if rawFold != cleanFold {
			tFull = math.Max(tClean, bestText(rawFold, titles...))
		}
		text := tClean
		cy := yearOf(c.Date)
		if year != "" {
			switch {
			case cy == year:
				text = tFull + 25
			case tFull >= 90:
				text = tFull
			default:
				text = tClean - 15
			}
		}
		if c.Via != "" {
			text = c.viaScore * 0.5
		}
		score := text
		score += math.Min(14, 4.5*math.Log10(1+c.Pop))
		score += math.Min(8, 1.6*math.Log10(1+float64(c.Votes)))
		if c.Poster == "" {
			score -= 6
		}
		if c.Overview == "" && c.Votes < 10 {
			score -= 8
		}
		if c.Votes == 0 && c.Pop < 1 {
			score -= 5
		}
		all = append(all, scored{
			r: Result{
				ID: c.ID, Type: c.Type, Title: c.Title, OriginalTitle: c.OrigTitle,
				Year: cy, Overview: trimRunes(c.Overview, 220), Poster: c.Poster,
				Backdrop: c.Backdrop, Rating: math.Round(c.Rating*10) / 10, Votes: c.Votes,
				Source: "TMDB", Via: c.Via, Score: math.Round(score*10) / 10,
			},
			text: text, pop: c.Pop,
		})
	}
	// Si hay buenas coincidencias, descarta el ruido.
	strong := 0
	for _, s := range all {
		if s.text >= 50 {
			strong++
		}
	}
	var out []scored
	for _, s := range all {
		if strong >= 3 && s.text < 25 {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].r.Score != out[j].r.Score {
			return out[i].r.Score > out[j].r.Score
		}
		return out[i].pop > out[j].pop
	})
	res := make([]Result, len(out))
	for i, s := range out {
		res[i] = s.r
	}
	return res
}

var sportsRe = regexp.MustCompile(`\b(vs|en vivo|en directo|directo|futbol|partido|liga|champions|mundial|copa|eliminatorias|nba|nfl|ufc|f1|formula 1|tenis|boxeo|beisbol|libertadores)\b`)

func looksSporty(f string) bool { return sportsRe.MatchString(f) }
