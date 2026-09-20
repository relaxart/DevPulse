package report

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"

	"github.com/go-pdf/fpdf"
)

// Landscape A4 in millimetres, so the wide contributor table fits on one page.
const (
	pageMargin  = 12.0
	pageWidth   = 297.0
	contentW    = pageWidth - 2*pageMargin
	lineH       = 5.0
	headerH     = 6.5
	fontFamily  = "Helvetica"
	chartHeight = 48.0
)

// Brand colours, matching the dashboard.
var (
	colorPrimary = [3]int{26, 115, 232}
	colorInk     = [3]int{31, 36, 48}
	colorMuted   = [3]int{95, 102, 114}
	colorRule    = [3]int{223, 227, 235}
	colorZebra   = [3]int{247, 249, 252}
	colorAdd     = [3]int{30, 142, 62}
	colorDel     = [3]int{217, 48, 37}
	colorReview  = [3]int{227, 116, 0}
	colorPR      = [3]int{132, 48, 206}
)

// column describes one table column.
type column struct {
	title string
	width float64
	align string
}

// Render produces the PDF document for a report.
func Render(d *Data) ([]byte, error) {
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.SetTitle(fmt.Sprintf("DevPulse activity report - %s", sanitize(d.Organization)), true)
	pdf.SetCreator("DevPulse", true)
	pdf.SetAutoPageBreak(true, 18)
	pdf.SetMargins(pageMargin, pageMargin, pageMargin)
	pdf.AliasNbPages("")

	r := &renderer{pdf: pdf, data: d}
	pdf.SetFooterFunc(r.footer)

	pdf.AddPage()
	r.title()
	r.overview()
	r.chart()
	r.contributors()
	r.repositories()
	r.glossary()

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render report pdf: %w", err)
	}
	return buf.Bytes(), nil
}

type renderer struct {
	pdf  *fpdf.Fpdf
	data *Data
}

func (r *renderer) setColor(c [3]int)     { r.pdf.SetTextColor(c[0], c[1], c[2]) }
func (r *renderer) setFillColor(c [3]int) { r.pdf.SetFillColor(c[0], c[1], c[2]) }
func (r *renderer) setDrawColor(c [3]int) { r.pdf.SetDrawColor(c[0], c[1], c[2]) }

func (r *renderer) footer() {
	r.pdf.SetY(-14)
	r.pdf.SetFont(fontFamily, "", 7.5)
	r.setColor(colorMuted)
	left := fmt.Sprintf("DevPulse - %s - %s (%s to %s, UTC days)",
		sanitize(r.data.Organization), sanitize(r.data.Range.Label),
		r.data.Range.FromString(), r.data.Range.ToString())
	r.pdf.CellFormat(contentW/2, 5, left, "", 0, "L", false, 0, "")
	r.pdf.CellFormat(contentW/2, 5, fmt.Sprintf("Page %d of {nb}", r.pdf.PageNo()), "", 0, "R", false, 0, "")

	r.pdf.SetY(-9)
	r.pdf.SetFont(fontFamily, "I", 7)
	r.pdf.CellFormat(contentW, 4,
		"Activity metrics are reported separately on purpose - DevPulse computes no overall performance score.",
		"", 0, "L", false, 0, "")
}

func (r *renderer) title() {
	p := r.pdf

	p.SetFont(fontFamily, "B", 18)
	r.setColor(colorInk)
	p.CellFormat(contentW, 9, "Engineering activity report", "", 1, "L", false, 0, "")

	p.SetFont(fontFamily, "", 11)
	r.setColor(colorPrimary)
	p.CellFormat(contentW, 6, sanitize(r.data.Organization), "", 1, "L", false, 0, "")

	p.SetFont(fontFamily, "", 9)
	r.setColor(colorMuted)
	period := fmt.Sprintf("%s  -  %s to %s (UTC days)",
		sanitize(r.data.Range.Label), r.data.Range.FromString(), r.data.Range.ToString())
	p.CellFormat(contentW, 5, period, "", 1, "L", false, 0, "")

	meta := fmt.Sprintf("Generated %s  -  Sorted by %s  -  Archived repositories %s",
		r.data.GeneratedAt.Format("2006-01-02 15:04 MST"),
		strings.ToLower(r.data.Sort.Label),
		archivedNote(r.data.IncludeArchived))
	if r.data.LastSync != nil {
		meta += "  -  Last sync " + r.data.LastSync.UTC().Format("2006-01-02 15:04 MST")
	}
	p.CellFormat(contentW, 5, meta, "", 1, "L", false, 0, "")

	if len(r.data.ExcludedUsers) > 0 {
		p.CellFormat(contentW, 5,
			"Excluded from rankings: "+sanitize(strings.Join(r.data.ExcludedUsers, ", "))+
				" (accounts GitHub reports as bots are excluded automatically)",
			"", 1, "L", false, 0, "")
	} else {
		p.CellFormat(contentW, 5,
			"Accounts GitHub reports as bots are excluded from rankings automatically.",
			"", 1, "L", false, 0, "")
	}

	if r.data.NoData {
		p.SetFont(fontFamily, "B", 9)
		r.setColor(colorDel)
		p.CellFormat(contentW, 5,
			"No data has been synchronized yet, so this report is empty. "+
				"Check the status page for synchronization errors.",
			"", 1, "L", false, 0, "")
	}

	p.Ln(2)
	r.rule()
	p.Ln(3)
}

func (r *renderer) rule() {
	y := r.pdf.GetY()
	r.setDrawColor(colorRule)
	r.pdf.SetLineWidth(0.2)
	r.pdf.Line(pageMargin, y, pageMargin+contentW, y)
}

func (r *renderer) sectionTitle(title string) {
	r.pdf.SetFont(fontFamily, "B", 11)
	r.setColor(colorInk)
	r.pdf.CellFormat(contentW, 7, title, "", 1, "L", false, 0, "")
}

// overview draws the six headline numbers as cards with a comparison line.
func (r *renderer) overview() {
	r.sectionTitle("Overview")

	type card struct {
		label   string
		value   int64
		compare int64
	}
	cards := []card{
		{"Active contributors", r.data.Overview.ActiveContributors, r.data.PreviousSum.ActiveContributors},
		{"Commits", r.data.Overview.Commits, r.data.PreviousSum.Commits},
		{"PRs opened", r.data.Overview.PRsOpened, r.data.PreviousSum.PRsOpened},
		{"PRs merged", r.data.Overview.PRsMerged, r.data.PreviousSum.PRsMerged},
		{"Reviews", r.data.Overview.Reviews, r.data.PreviousSum.Reviews},
		{"Active repositories", r.data.Overview.ActiveRepositories, r.data.PreviousSum.ActiveRepositories},
	}

	const gap = 3.0
	w := (contentW - gap*float64(len(cards)-1)) / float64(len(cards))
	h := 20.0
	y := r.pdf.GetY()

	for i, c := range cards {
		x := pageMargin + float64(i)*(w+gap)
		r.setFillColor(colorZebra)
		r.setDrawColor(colorRule)
		r.pdf.SetLineWidth(0.2)
		r.pdf.Rect(x, y, w, h, "FD")

		r.pdf.SetXY(x+3, y+2.5)
		r.pdf.SetFont(fontFamily, "B", 15)
		r.setColor(colorInk)
		r.pdf.CellFormat(w-6, 7, formatNumber(c.value), "", 2, "L", false, 0, "")

		r.pdf.SetFont(fontFamily, "", 7.5)
		r.setColor(colorMuted)
		r.pdf.CellFormat(w-6, 4, c.label, "", 2, "L", false, 0, "")
		r.pdf.CellFormat(w-6, 4, formatNumber(c.compare)+" in previous period", "", 2, "L", false, 0, "")
	}

	r.pdf.SetY(y + h + 3)
	r.pdf.SetFont(fontFamily, "", 8)
	r.setColor(colorMuted)
	net := r.data.Overview.Additions - r.data.Overview.Deletions
	r.pdf.CellFormat(contentW, 4.5, fmt.Sprintf(
		"Lines: +%s / -%s (net %s).  Previous period: %s to %s.",
		formatNumber(r.data.Overview.Additions), formatNumber(r.data.Overview.Deletions), formatSigned(net),
		r.data.Previous.FromString(), r.data.Previous.ToString()), "", 1, "L", false, 0, "")
	r.pdf.Ln(2)
}

// chart draws a grouped bar chart of commits, pull requests and reviews.
func (r *renderer) chart() {
	r.sectionTitle(fmt.Sprintf("Activity over time (%s buckets)", r.data.Granularity))

	p := r.pdf
	x := pageMargin
	y := p.GetY()
	w := contentW
	h := chartHeight

	if len(r.data.Series) == 0 {
		p.SetFont(fontFamily, "I", 9)
		r.setColor(colorMuted)
		p.CellFormat(contentW, 8, "No activity recorded in this period.", "", 1, "L", false, 0, "")
		p.Ln(2)
		return
	}

	var max int64 = 1
	for _, pt := range r.data.Series {
		for _, v := range []int64{pt.Commits, pt.PRsOpened, pt.ReviewsSubmitted} {
			if v > max {
				max = v
			}
		}
	}

	plotTop := y + 2
	plotH := h - 10
	plotBottom := plotTop + plotH

	// Horizontal grid lines with value labels.
	r.setDrawColor(colorRule)
	p.SetLineWidth(0.15)
	p.SetFont(fontFamily, "", 6.5)
	r.setColor(colorMuted)
	for i := 0; i <= 4; i++ {
		gy := plotBottom - plotH*float64(i)/4
		p.Line(x+12, gy, x+w, gy)
		p.SetXY(x, gy-2)
		p.CellFormat(10, 4, formatNumber(max*int64(i)/4), "", 0, "R", false, 0, "")
	}

	plotX := x + 12
	plotW := w - 12
	slot := plotW / float64(len(r.data.Series))
	barW := slot / 4.2
	if barW > 6 {
		barW = 6
	}

	for i, pt := range r.data.Series {
		base := plotX + float64(i)*slot + (slot-3*barW)/2
		values := []struct {
			v int64
			c [3]int
		}{
			{pt.Commits, colorPrimary},
			{pt.PRsOpened, colorPR},
			{pt.ReviewsSubmitted, colorReview},
		}
		for j, v := range values {
			if v.v <= 0 {
				continue
			}
			bh := plotH * float64(v.v) / float64(max)
			if bh < 0.4 {
				bh = 0.4
			}
			r.setFillColor(v.c)
			p.Rect(base+float64(j)*barW, plotBottom-bh, barW*0.9, bh, "F")
		}
	}

	// Axis line and a readable subset of bucket labels.
	r.setDrawColor(colorRule)
	p.SetLineWidth(0.3)
	p.Line(plotX, plotBottom, x+w, plotBottom)

	step := 1
	for len(r.data.Series)/step > 12 {
		step++
	}
	p.SetFont(fontFamily, "", 6)
	r.setColor(colorMuted)
	for i, pt := range r.data.Series {
		if i%step != 0 && i != len(r.data.Series)-1 {
			continue
		}
		p.SetXY(plotX+float64(i)*slot-6, plotBottom+1.5)
		p.CellFormat(slot+12, 4, pt.Label, "", 0, "C", false, 0, "")
	}

	p.SetY(plotBottom + 6)
	r.legend([]struct {
		label string
		color [3]int
	}{
		{"Commits", colorPrimary},
		{"PRs opened", colorPR},
		{"Reviews submitted", colorReview},
	})
	p.Ln(3)
}

func (r *renderer) legend(items []struct {
	label string
	color [3]int
}) {
	p := r.pdf
	y := p.GetY()
	x := pageMargin
	p.SetFont(fontFamily, "", 7.5)
	for _, item := range items {
		r.setFillColor(item.color)
		p.Rect(x, y+1, 3, 3, "F")
		r.setColor(colorMuted)
		p.SetXY(x+4.5, y)
		wText := p.GetStringWidth(item.label) + 8
		p.CellFormat(wText, 5, item.label, "", 0, "L", false, 0, "")
		x += wText + 5
	}
	p.SetY(y + 5)
}

// contributorColumns is the ranking table layout. Widths sum to contentW.
func contributorColumns() []column {
	// Widths must sum to contentW; TestTableWidthsFitThePage enforces it.
	return []column{
		{"#", 7, "R"},
		{"Contributor", 45, "L"},
		{"Commits", 17, "R"},
		{"Additions", 20, "R"},
		{"Deletions", 20, "R"},
		{"Files", 15, "R"},
		{"PRs op.", 15, "R"},
		{"Merged", 15, "R"},
		{"Closed", 14, "R"},
		{"Reviews", 16, "R"},
		{"Appr.", 13, "R"},
		{"Ch.req.", 15, "R"},
		{"R.comm.", 16, "R"},
		{"PRs rev.", 16, "R"},
		{"Act.days", 16, "R"},
		{"Repos", 13, "R"},
	}
}

func (r *renderer) contributors() {
	r.pageBreakIfNeeded(40)
	r.sectionTitle(fmt.Sprintf("Top contributors - sorted by %s", strings.ToLower(r.data.Sort.Label)))

	cols := contributorColumns()
	if len(r.data.Contributors) == 0 {
		r.emptyRow("No contributor activity in this period.")
		return
	}

	r.tableHeader(cols)
	for i, c := range r.data.Contributors {
		r.rowBreak(cols)
		name := sanitize(c.DisplayName())
		if c.Login != "" && !strings.EqualFold(name, c.Login) {
			name = fmt.Sprintf("%s (@%s)", name, c.Login)
		} else {
			name = "@" + c.Login
		}
		r.row(cols, i, []string{
			fmt.Sprintf("%d", i+1),
			truncateTo(r.pdf, name, cols[1].width-2),
			formatNumber(c.Commits),
			plus(c.Additions),
			minus(c.Deletions),
			formatNumber(c.ChangedFiles),
			formatNumber(c.PRsOpened),
			formatNumber(c.PRsMerged),
			formatNumber(c.PRsClosed),
			formatNumber(c.ReviewsSubmitted),
			formatNumber(c.Approvals),
			formatNumber(c.ChangesRequested),
			formatNumber(c.ReviewComments),
			formatNumber(c.UniquePRsReviewed),
			formatNumber(c.ActiveDays),
			formatNumber(c.Repositories),
		}, map[int][3]int{3: colorAdd, 4: colorDel})
	}
	if r.data.ContributorsTruncated {
		r.note(fmt.Sprintf("Showing the top %d contributors by %s; more exist.",
			len(r.data.Contributors), strings.ToLower(r.data.Sort.Label)))
	}
	r.pdf.Ln(3)
}

func repositoryColumns() []column {
	// Widths must sum to contentW; TestTableWidthsFitThePage enforces it.
	return []column{
		{"Repository", 55, "L"},
		{"Language", 22, "L"},
		{"Status", 20, "L"},
		{"Contributors", 22, "R"},
		{"Commits", 20, "R"},
		{"Additions", 22, "R"},
		{"Deletions", 22, "R"},
		{"PRs opened", 22, "R"},
		{"PRs merged", 22, "R"},
		{"Reviews", 18, "R"},
		{"Last activity", 28, "R"},
	}
}

func (r *renderer) repositories() {
	r.pageBreakIfNeeded(40)
	r.sectionTitle("Repositories")

	cols := repositoryColumns()
	if len(r.data.Repositories) == 0 {
		r.emptyRow("No repositories have been synchronized yet.")
		return
	}

	r.tableHeader(cols)
	for i, repo := range r.data.Repositories {
		r.rowBreak(cols)
		status := "active"
		if repo.IsArchived {
			status = "archived"
		}
		if repo.IsPrivate {
			status += ", private"
		}
		last := "never"
		if repo.LastActivity != nil {
			last = repo.LastActivity.UTC().Format("2006-01-02")
		}
		r.row(cols, i, []string{
			truncateTo(r.pdf, sanitize(repo.Name), cols[0].width-2),
			truncateTo(r.pdf, sanitize(repo.PrimaryLang), cols[1].width-2),
			status,
			formatNumber(repo.Contributors),
			formatNumber(repo.Commits),
			plus(repo.Additions),
			minus(repo.Deletions),
			formatNumber(repo.PRsOpened),
			formatNumber(repo.PRsMerged),
			formatNumber(repo.Reviews),
			last,
		}, map[int][3]int{5: colorAdd, 6: colorDel})
	}
	if r.data.RepositoriesTruncated {
		r.note(fmt.Sprintf("Showing %d repositories; more exist.", len(r.data.Repositories)))
	}
	r.pdf.Ln(3)
}

func (r *renderer) glossary() {
	r.pdf.AddPage()
	r.sectionTitle("What each metric means")

	r.pdf.SetFont(fontFamily, "", 8)
	r.setColor(colorMuted)
	r.pdf.MultiCell(contentW, 4.2,
		"DevPulse reports observable engineering activity. It deliberately does not combine these numbers "+
			"into a single score and does not judge whether a contributor is productive: different metrics "+
			"describe different kinds of contribution and are meant to be read and compared independently. "+
			"Commit metrics are attributed to the commit author, pull request metrics to the pull request "+
			"author, and review metrics to the reviewer. All days are UTC calendar days.",
		"", "L", false)
	r.pdf.Ln(2)

	for _, m := range r.data.Metrics {
		r.pageBreakIfNeeded(14)
		x := r.pdf.GetX()
		y := r.pdf.GetY()

		r.pdf.SetFont(fontFamily, "B", 8)
		r.setColor(colorInk)
		r.pdf.CellFormat(45, 4.6, sanitize(m.Label), "", 0, "L", false, 0, "")

		r.pdf.SetFont(fontFamily, "", 8)
		r.setColor(colorMuted)
		r.pdf.SetXY(x+45, y)
		r.pdf.MultiCell(contentW-45, 4.6, sanitize(m.Description), "", "L", false)
		r.pdf.Ln(0.6)
	}
}

// ---------------------------------------------------------------------------
// table helpers
// ---------------------------------------------------------------------------

func (r *renderer) tableHeader(cols []column) {
	p := r.pdf
	p.SetFont(fontFamily, "B", 7)
	r.setColor(colorMuted)
	r.setFillColor(colorZebra)
	r.setDrawColor(colorRule)
	p.SetLineWidth(0.2)
	for _, c := range cols {
		p.CellFormat(c.width, headerH, c.title, "B", 0, c.align, true, 0, "")
	}
	p.Ln(-1)
}

// rowBreak starts a new page and repeats the table header when the current page
// is full, so a table never continues headerless.
func (r *renderer) rowBreak(cols []column) {
	if r.pdf.GetY() > 210-18-lineH {
		r.pdf.AddPage()
		r.tableHeader(cols)
	}
}

func (r *renderer) row(cols []column, index int, values []string, colored map[int][3]int) {
	p := r.pdf
	p.SetFont(fontFamily, "", 7.5)
	fill := index%2 == 1
	if fill {
		r.setFillColor(colorZebra)
	}
	for i, c := range cols {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		if col, ok := colored[i]; ok {
			r.setColor(col)
		} else {
			r.setColor(colorInk)
		}
		p.CellFormat(c.width, lineH, v, "", 0, c.align, fill, 0, "")
	}
	p.Ln(-1)
}

func (r *renderer) emptyRow(text string) {
	r.pdf.SetFont(fontFamily, "I", 9)
	r.setColor(colorMuted)
	r.pdf.CellFormat(contentW, 8, text, "", 1, "L", false, 0, "")
	r.pdf.Ln(2)
}

func (r *renderer) note(text string) {
	r.pdf.Ln(1)
	r.pdf.SetFont(fontFamily, "I", 7.5)
	r.setColor(colorMuted)
	r.pdf.CellFormat(contentW, 4.5, text, "", 1, "L", false, 0, "")
}

func (r *renderer) pageBreakIfNeeded(needed float64) {
	if r.pdf.GetY()+needed > 210-18 {
		r.pdf.AddPage()
	}
}

// truncateTo shortens text with an ellipsis so it fits the given width.
func truncateTo(pdf *fpdf.Fpdf, s string, width float64) string {
	if pdf.GetStringWidth(s) <= width {
		return s
	}
	for len(s) > 1 {
		s = s[:len(s)-1]
		if pdf.GetStringWidth(s+"...") <= width {
			return s + "..."
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// formatting
// ---------------------------------------------------------------------------

func formatNumber(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d", v)
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

// plus and minus render diff counters. A zero is shown plainly rather than as
// "+0" / "-0", which reads like a mistake in a table.
func plus(v int64) string {
	if v == 0 {
		return "0"
	}
	return "+" + formatNumber(v)
}

func minus(v int64) string {
	if v == 0 {
		return "0"
	}
	return "-" + formatNumber(v)
}

func formatSigned(v int64) string {
	if v > 0 {
		return "+" + formatNumber(v)
	}
	return formatNumber(v)
}

func archivedNote(included bool) string {
	if included {
		return "included"
	}
	return "excluded"
}

// sanitize maps GitHub-provided text onto the Latin-1 range the built-in PDF
// fonts can draw. Characters outside it (CJK, emoji) become '?', which keeps a
// display name readable rather than dropping it; the ASCII @login shown beside
// it always survives.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20:
			// Control characters are dropped.
		case r < 0x7f:
			b.WriteRune(r)
		case r >= 0xa0 && r <= 0xff:
			// The core fonts expect single-byte Latin-1, not UTF-8.
			b.WriteByte(byte(r))
		case r == '‘' || r == '’':
			b.WriteByte('\'')
		case r == '“' || r == '”':
			b.WriteByte('"')
		case r == '–' || r == '—' || r == '−':
			b.WriteByte('-')
		case r == '…':
			b.WriteString("...")
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}
