package pulse

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Width is how much of the page row a card takes.
type Width int

const (
	Full Width = iota
	Half
	Third
)

// Card is one panel of a dashboard page. Render is called on every page load
// and refresh and returns one of the widgets in this package.
type Card struct {
	Title  string
	Width  Width
	Render func(ctx context.Context, v View) (Widget, error)
}

// Page is a dashboard tab made of cards.
type Page struct {
	Title string
	Slug  string
	// Hidden pages are reachable by URL but not listed in the navigation.
	Hidden bool
	// Parent is the slug of the tab highlighted while a hidden page is open.
	Parent string
	Cards  []Card
}

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(reSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// AddPage registers a custom dashboard tab.
func (p *Pulse) AddPage(title string, cards ...Card) *Page {
	return p.addPage(&Page{Title: title, Cards: cards})
}

func (p *Pulse) addPage(page *Page) *Page {
	if page.Slug == "" {
		page.Slug = slugify(page.Title)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.pages {
		if existing.Slug == page.Slug {
			p.pages[i] = page
			return page
		}
	}
	p.pages = append(p.pages, page)
	return page
}

func (p *Pulse) page(slug string) *Page {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if slug == "" && len(p.pages) > 0 {
		return p.pages[0]
	}
	for _, page := range p.pages {
		if page.Slug == slug {
			return page
		}
	}
	return nil
}

// View is the time window and request parameters a card renders for.
type View struct {
	From   time.Time
	To     time.Time
	Period string
	Params url.Values

	base string
}

// Param returns a URL query parameter of the current page request.
func (v View) Param(name string) string { return v.Params.Get(name) }

// PageURL builds a link to another page, keeping the selected period.
// Extra query parameters are given as key, value pairs.
func (v View) PageURL(slug string, kv ...string) string {
	q := url.Values{}
	if v.Period != "" {
		q.Set("period", v.Period)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	u := v.base + "/" + slug
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// Widget is the content of a card. Use Stats, Table, TimeSeries, KeyValue,
// Pre or HTML.
type Widget interface {
	render(t *template.Template) (template.HTML, error)
}

func execWidget(t *template.Template, name string, data any) (template.HTML, error) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// Tone colours a value by state.
type Tone string

const (
	ToneNone Tone = ""
	ToneGood Tone = "good"
	ToneWarn Tone = "warn"
	ToneBad  Tone = "bad"
)

// Stat is a single headline number.
type Stat struct {
	Label string
	Value string
	Hint  string
	Tone  Tone
	// Meter, when set, draws a 0-100 gauge under the value.
	Meter *float64
}

// Percent is a helper for Stat.Meter and Cell.Meter.
func Percent(v float64) *float64 { return &v }

// Stats is a row of headline numbers.
type Stats []Stat

func (s Stats) render(t *template.Template) (template.HTML, error) {
	return execWidget(t, "stats", s)
}

// Cell is a table cell with optional link, alignment and state.
type Cell struct {
	Text  string
	Href  string
	Title string
	Right bool
	Mono  bool
	Tone  Tone
	Meter *float64
}

// Num is a right-aligned cell for numbers.
func Num(text string) Cell { return Cell{Text: text, Right: true} }

// Link is a cell that links to href.
func Link(text, href string) Cell { return Cell{Text: text, Href: href} }

// Table is a simple grid. Row values may be Cell or anything fmt can print.
type Table struct {
	Columns []string
	// ColumnHrefs optionally turns column titles into links, e.g. for sorting.
	ColumnHrefs []string
	Rows        [][]any
	// Empty is shown instead of the table when there are no rows.
	Empty string
}

type tableColumn struct {
	Title string
	Href  string
	Right bool
}

type tableVM struct {
	Columns []tableColumn
	Rows    [][]Cell
	Empty   string
}

func (tb Table) render(t *template.Template) (template.HTML, error) {
	vm := tableVM{Empty: tb.Empty}
	if vm.Empty == "" {
		vm.Empty = "Nothing recorded yet."
	}
	for i, c := range tb.Columns {
		col := tableColumn{Title: c}
		if i < len(tb.ColumnHrefs) {
			col.Href = tb.ColumnHrefs[i]
		}
		vm.Columns = append(vm.Columns, col)
	}
	for ri, row := range tb.Rows {
		cells := make([]Cell, len(row))
		for i, v := range row {
			if c, ok := v.(Cell); ok {
				cells[i] = c
			} else {
				cells[i] = Cell{Text: fmt.Sprint(v)}
			}
			if ri == 0 && i < len(vm.Columns) {
				vm.Columns[i].Right = cells[i].Right
			}
		}
		vm.Rows = append(vm.Rows, cells)
	}
	return execWidget(t, "table", vm)
}

// KV is one row of a KeyValue widget.
type KV struct {
	Key   string
	Value string
}

// KeyValue is a list of labelled values.
type KeyValue []KV

func (kv KeyValue) render(t *template.Template) (template.HTML, error) {
	return execWidget(t, "keyvalue", kv)
}

// Pre shows preformatted text such as a stack trace.
type Pre string

func (p Pre) render(t *template.Template) (template.HTML, error) {
	return execWidget(t, "pre", string(p))
}

// HTML is raw markup supplied by the card. It is not escaped.
type HTML template.HTML

func (h HTML) render(*template.Template) (template.HTML, error) {
	return template.HTML(h), nil
}

type renderedCard struct {
	Title string
	Class string
	Body  template.HTML
	Error string
}

func (p *Pulse) renderCards(ctx context.Context, page *Page, v View) []renderedCard {
	out := make([]renderedCard, len(page.Cards))
	for i, c := range page.Cards {
		rc := renderedCard{Title: c.Title, Class: [...]string{"full", "half", "third"}[c.Width]}
		body, err := p.renderCard(ctx, c, v)
		if err != nil {
			rc.Error = err.Error()
		}
		rc.Body = body
		out[i] = rc
	}
	return out
}

func (p *Pulse) renderCard(ctx context.Context, c Card, v View) (body template.HTML, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("card panicked: %v", r)
		}
	}()
	if c.Render == nil {
		return "", nil
	}
	w, err := c.Render(ctx, v)
	if err != nil || w == nil {
		return "", err
	}
	return w.render(p.tmpl)
}
