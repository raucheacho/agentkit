package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec dit quand une tâche revient.
type Spec interface {
	// Next renvoie la première exécution strictement après t ; zéro s'il n'y
	// en a plus.
	Next(t time.Time) time.Time
	// String renvoie la forme canonique, relue par Parse.
	String() string
}

// Parse lit une planification, dans le fuseau loc :
//   - « every 30m », « every 2h », « every 1d » : à intervalle fixe ;
//   - expression cron à cinq champs (« 0 9 * * 1-5 ») ou @hourly, @daily,
//     @weekly, @monthly ;
//   - une date (« 2026-12-01 09:00 », RFC 3339) : une seule fois ;
//   - « in 20m », « in 2h » : une seule fois, à partir de now.
func Parse(s string, loc *time.Location, now time.Time) (Spec, error) {
	if loc == nil {
		loc = time.Local
	}
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	switch {
	case low == "":
		return nil, errors.New("planification vide")
	case strings.HasPrefix(low, "every "):
		d, err := parseDuration(strings.TrimSpace(low[6:]))
		if err != nil {
			return nil, err
		}
		return every{d: d}, nil
	case strings.HasPrefix(low, "in "):
		d, err := parseDuration(strings.TrimSpace(low[3:]))
		if err != nil {
			return nil, err
		}
		return once{at: now.Add(d).Truncate(time.Second)}, nil
	case strings.HasPrefix(low, "at "):
		return parseAt(s[3:], loc)
	case strings.HasPrefix(low, "@") || strings.Count(strings.TrimSpace(low), " ") >= 4 && !strings.Contains(low, ":"):
		return parseCron(low, loc)
	}
	return parseAt(s, loc)
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.ReplaceAll(s, " ", "")
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("durée invalide %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("durée invalide %q (ex. 30m, 2h, 1d)", s)
	}
	return d, nil
}

func parseAt(s string, loc *time.Location) (Spec, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return once{at: t}, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return once{at: t}, nil
		}
	}
	return nil, fmt.Errorf("planification %q illisible : attendu « every 2h », une expression cron « 0 9 * * 1-5 », "+
		"une date « 2026-12-01 09:00 » ou « in 20m »", s)
}

type every struct{ d time.Duration }

func (e every) Next(t time.Time) time.Time { return t.Add(e.d) }
func (e every) String() string {
	if e.d%(24*time.Hour) == 0 {
		return fmt.Sprintf("every %dd", e.d/(24*time.Hour))
	}
	var b strings.Builder
	b.WriteString("every ")
	h, m, sec := e.d/time.Hour, (e.d%time.Hour)/time.Minute, e.d%time.Minute
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if sec > 0 {
		b.WriteString(sec.String())
	}
	return b.String()
}

type once struct{ at time.Time }

func (o once) Next(t time.Time) time.Time {
	if t.Before(o.at) {
		return o.at
	}
	return time.Time{}
}
func (o once) String() string { return "at " + o.at.Format(time.RFC3339) }

// cron : champs minute, heure, jour du mois, mois, jour de la semaine.
type cron struct {
	src                      string
	min, hour, dom, mon, dow uint64
	domStar, dowStar         bool
	loc                      *time.Location
}

var macros = map[string]string{
	"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *",
	"@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
}

func parseCron(s string, loc *time.Location) (Spec, error) {
	expr := s
	if m, ok := macros[s]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("expression cron %q : cinq champs attendus (minute heure jour mois jour-de-semaine)", s)
	}
	c := cron{src: strings.Join(strings.Fields(s), " "), loc: loc}
	var err error
	ranges := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	dest := []*uint64{&c.min, &c.hour, &c.dom, &c.mon, &c.dow}
	for i, field := range f {
		if *dest[i], err = parseField(field, ranges[i][0], ranges[i][1]); err != nil {
			return nil, fmt.Errorf("expression cron %q : %w", s, err)
		}
	}
	if c.dow&(1<<7) != 0 { // 7 = dimanche
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*", f[4] == "*"
	return c, nil
}

func parseField(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("pas invalide %q", part)
			}
			step = n
		}
		a, b := lo, hi
		if rng != "*" {
			x, y, isRange := strings.Cut(rng, "-")
			var err error
			if a, err = strconv.Atoi(x); err != nil {
				return 0, fmt.Errorf("valeur invalide %q", part)
			}
			b = a
			if isRange {
				if b, err = strconv.Atoi(y); err != nil {
					return 0, fmt.Errorf("valeur invalide %q", part)
				}
			} else if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("valeur hors limites %q (%d-%d)", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func has(bits uint64, v int) bool { return bits&(1<<v) != 0 }

func (c cron) dayMatches(t time.Time) bool {
	d, w := has(c.dom, t.Day()), has(c.dow, int(t.Weekday()))
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return w
	case c.dowStar:
		return d
	}
	return d || w // convention cron : l'un ou l'autre
}

func (c cron) Next(after time.Time) time.Time {
	t := after.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case !has(c.mon, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, c.loc)
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, c.loc)
		case !has(c.hour, t.Hour()):
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, c.loc)
		case !has(c.min, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

func (c cron) String() string { return c.src }
