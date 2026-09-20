package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/relaxart/dev-pulse/internal/metrics"
)

// renderer holds the parsed page templates.
type renderer struct {
	pages map[string]*template.Template
}

// pageNames are the templates composed with layout.html and the partials.
var pageNames = []string{
	"dashboard.html",
	"contributors.html",
	"contributor.html",
	"repositories.html",
	"repository.html",
	"status.html",
	"error.html",
}

// newRenderer parses every page template against the shared layout.
func newRenderer(fsys fs.FS) (*renderer, error) {
	r := &renderer{pages: make(map[string]*template.Template, len(pageNames))}
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(templateFuncs()).ParseFS(fsys,
			"templates/layout.html",
			"templates/partials.html",
			"templates/"+name,
		)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		r.pages[name] = t
	}
	return r, nil
}

// render executes a page. It buffers first so a template error cannot emit a
// half-written page with a 200 status.
func (r *renderer) render(w http.ResponseWriter, status int, page string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("unknown template %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		return fmt.Errorf("render %s: %w", page, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := buf.WriteTo(w)
	return err
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"num":         formatNumber,
		"compact":     formatCompact,
		"signed":      formatSigned,
		"date":        formatDate,
		"datetime":    formatDateTime,
		"ago":         humanizeAgo,
		"duration":    formatDuration,
		"json":        toJSON,
		"link":        link,
		"sortLink":    sortLink,
		"pageLink":    pageLink,
		"dict":        dict,
		"initials":    initials,
		"stateBadge":  stateBadge,
		"pct":         percent,
		"pct2":        percentChange,
		"sub":         func(a, b int64) int64 { return a - b },
		"extraParams": extraParams,
		"lower":       strings.ToLower,
		"add":         func(a, b int) int { return a + b },
	}
}

// formatNumber renders an integer with thousands separators. It accepts the
// several integer widths that reach templates (counts, ids, page numbers).
func formatNumber(v any) string {
	n, ok := toInt64(v)
	if !ok {
		return fmt.Sprint(v)
	}
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// toInt64 normalises the integer types that reach the template layer.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint64:
		return int64(n), true
	default:
		return 0, false
	}
}

// formatCompact renders large counts as 12.4k / 3.1M.
func formatCompact(value any) string {
	v, ok := toInt64(value)
	if !ok {
		return fmt.Sprint(value)
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var s string
	switch {
	case v >= 1_000_000:
		s = fmt.Sprintf("%.1fM", float64(v)/1_000_000)
	case v >= 1_000:
		s = fmt.Sprintf("%.1fk", float64(v)/1_000)
	default:
		s = fmt.Sprintf("%d", v)
	}
	s = strings.Replace(s, ".0", "", 1)
	if neg {
		return "-" + s
	}
	return s
}

// formatSigned renders a net line change such as "+12.4k".
func formatSigned(value any) string {
	v, ok := toInt64(value)
	if !ok {
		return fmt.Sprint(value)
	}
	if v > 0 {
		return "+" + formatCompact(v)
	}
	return formatCompact(v)
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02")
}

func formatDateTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}

func humanizeAgo(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < 0:
		return "in " + formatDuration(-d)
	case d < time.Minute:
		return "just now"
	default:
		return formatDuration(d) + " ago"
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func percent(part, total int64) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.0f%%", float64(part)/float64(total)*100)
}

// jsEscaper neutralises sequences that could break out of a <script> block.
var jsEscaper = strings.NewReplacer(
	"<", `<`,
	">", `>`,
	"&", `&`,
	" ", ` `,
	" ", ` `,
)

// toJSON marshals chart data for embedding in a <script> block. Every
// problematic character is escaped, so GitHub-provided strings (repository
// names, pull request titles) cannot terminate the script element.
func toJSON(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(jsEscaper.Replace(string(b)))
}

// KV is one query parameter carried through a GET form as a hidden input.
type KV struct {
	Key   string
	Value string
}

// extraParams lists the query parameters the period form must preserve (for
// example the contributor sort key), excluding the ones the form owns itself.
func extraParams(q url.Values) []KV {
	owned := map[string]bool{"period": true, "from": true, "to": true}
	keys := make([]string, 0, len(q))
	for k := range q {
		if !owned[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, KV{Key: k, Value: q.Get(k)})
	}
	return out
}

// percentChange renders a period-over-period delta.
func percentChange(delta, previous int64) string {
	if previous == 0 {
		return "n/a"
	}
	p := float64(delta) / float64(previous) * 100
	if p < 0 {
		p = -p
	}
	return fmt.Sprintf("%.0f%%", p)
}

// link builds a URL that carries the current filter query.
func link(path string, q url.Values) template.URL {
	if len(q) == 0 {
		return template.URL(path)
	}
	return template.URL(path + "?" + q.Encode())
}

// sortLink builds a contributor ranking link for one sort key, preserving the
// current date filter.
func sortLink(path string, q url.Values, sortKey string) template.URL {
	v := cloneValues(q)
	if metrics.IsValidSort(sortKey) {
		v.Set("sort", sortKey)
	}
	return template.URL(path + "?" + v.Encode())
}

// pageLink builds a ranking link for one pagination page, keeping the filter.
func pageLink(path string, q url.Values, page int) template.URL {
	v := cloneValues(q)
	if page > 1 {
		v.Set("page", fmt.Sprintf("%d", page))
	} else {
		v.Del("page")
	}
	if len(v) == 0 {
		return template.URL(path)
	}
	return template.URL(path + "?" + v.Encode())
}

// dict builds a map inside a template, for partial invocation.
func dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, fmt.Errorf("dict needs an even number of arguments")
	}
	m := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict keys must be strings")
		}
		m[key] = values[i+1]
	}
	return m, nil
}

// initials renders an avatar fallback.
func initials(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "?"
	}
	parts := strings.Fields(name)
	if len(parts) == 1 {
		return strings.ToUpper(parts[0][:1])
	}
	return strings.ToUpper(parts[0][:1] + parts[1][:1])
}

// stateBadge maps a pull request or review state to a Bootstrap colour.
func stateBadge(state string) string {
	switch strings.ToUpper(state) {
	case "MERGED", "APPROVED", "SUCCESS":
		return "success"
	case "OPEN", "RUNNING":
		return "primary"
	case "CHANGES_REQUESTED", "PARTIAL":
		return "warning"
	case "FAILED":
		return "danger"
	case "CLOSED", "DISMISSED":
		return "secondary"
	default:
		return "info"
	}
}
