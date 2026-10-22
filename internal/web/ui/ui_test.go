package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/a-h/templ"
	templruntime "github.com/a-h/templ/runtime"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

var errWrite = errors.New("write refused")

// failAfter accepts n writes, then refuses every one.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errWrite
	}
	f.n--
	return len(p), nil
}

func shell() Shell {
	return Shell{Title: i18n.HomeTitle, Current: "/", CSS: "/static/css/workspace.0.css", Script: "/static/vendor/htmx.0.js"}
}

// components are every component, rendered alone.
func components() map[string]templ.Component {
	return map[string]templ.Component{
		"Layout":          Layout(shell(), Value("content")),
		"Notice":          Notice("n", i18n.HomeNotice, Value("message")),
		"SummaryList":     SummaryList([]Row{{Key: i18n.StatusState, Value: Value("v")}}),
		"Tag":             Tag(i18n.TagPlanned, ""),
		"ButtonLink":      ButtonLink("/", i18n.NotFoundHome),
		"Code":            Code("v1.0.0"),
		"Paragraph":       Paragraph(i18n.HomeJobsCount, 2),
		"Home":            Home(shell()),
		"homeContent":     homeContent(),
		"StatusPage":      StatusPage(shell(), Status{Version: "v1.2.3", Revision: "abc"}),
		"statusContent":   statusContent(Status{Version: "devel"}),
		"NotFound":        NotFound(shell()),
		"notFoundContent": notFoundContent(),
	}
}

func locale(t *testing.T) context.Context {
	t.Helper()
	c, err := i18n.Load(false)
	if err != nil {
		t.Fatal(err)
	}
	return templ.WithNonce(i18n.With(context.Background(), c.Default()), "nonce-value")
}

// TestComponentsRender renders every component as a page would (into a
// plain writer, also with nil children) and then into a writer that refuses its first, second, …
// write until one succeeds, so every write's error path in the generated
// code runs and returns the error; and with a cancelled context, which
// every component refuses before writing.
func TestComponentsRender(t *testing.T) {
	ctx := locale(t)
	size := templruntime.DefaultBufferSize
	// One byte: every write reaches the failing writer at once.
	templruntime.DefaultBufferSize = 1
	t.Cleanup(func() { templruntime.DefaultBufferSize = size })
	for name, c := range components() {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := c.Render(ctx, &out); err != nil || out.Len() == 0 {
				t.Fatalf("render: %v, %d bytes", err, out.Len())
			}
			// templ's generated code replaces nil children with none.
			if err := c.Render(templ.WithChildren(ctx, nil), &out); err != nil {
				t.Errorf("render with nil children: %v", err)
			}
			if err := c.Render(ctx, &failAfter{}); !errors.Is(err, errWrite) {
				t.Errorf("render into a refusing writer: got %v, want the write's error", err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := c.Render(cancelled, &out); !errors.Is(err, context.Canceled) {
				t.Errorf("cancelled: got %v, want context.Canceled", err)
			}
			for n := 0; ; n++ {
				if n > 10000 {
					t.Fatal("no render succeeded within 10000 writes")
				}
				var b templruntime.Buffer
				b.Reset(&failAfter{n: n})
				err := c.Render(ctx, &b)
				if err == nil {
					err = b.Flush()
				}
				if err == nil {
					break
				}
				if !errors.Is(err, errWrite) {
					t.Fatalf("write %d: got %v, want the write's error", n, err)
				}
			}
		})
	}
}

// attr is the value of an element's attribute.
func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// find is the first element with tag below n.
func find(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := find(c, tag); f != nil {
			return f
		}
	}
	return nil
}

func parse(t *testing.T, ctx context.Context, c templ.Component) *html.Node {
	t.Helper()
	var out bytes.Buffer
	if err := c.Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(&out)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestLayout(t *testing.T) {
	doc := parse(t, locale(t), Home(shell()))
	root := find(doc, "html")
	for key, want := range map[string]string{"lang": "en", "dir": "ltr"} {
		if got, _ := attr(root, key); got != want {
			t.Errorf("html %s: got %q, want %q", key, got, want)
		}
	}
	script := find(doc, "script")
	for key, want := range map[string]string{"src": "/static/vendor/htmx.0.js", "nonce": "nonce-value", "defer": ""} {
		if got, ok := attr(script, key); !ok || got != want {
			t.Errorf("script %s: got %q (%v), want %q", key, got, ok, want)
		}
	}
	nav := find(find(doc, "nav"), "a")
	if got, _ := attr(nav, "aria-current"); got != "page" {
		t.Errorf("the current page's navigation entry: aria-current %q, want page", got)
	}
	if got := find(doc, "title").FirstChild.Data; got != "Your workspace – ERP workspace" {
		t.Errorf("title: got %q", got)
	}
}

// TestNoLocale: outside a request with a locale the page still says its
// language, English, and shows keys rather than failing.
func TestNoLocale(t *testing.T) {
	doc := parse(t, context.Background(), Tag(i18n.TagPlanned, "success"))
	strong := find(doc, "strong")
	if got, _ := attr(strong, "class"); got != "tag tag--success" {
		t.Errorf("class: got %q, want tag tag--success", got)
	}
	if got := strong.FirstChild.Data; got != string(i18n.TagPlanned) {
		t.Errorf("text: got %q, want the key", got)
	}
	if got := htmlAttrs(context.Background()); got["lang"] != "en" || got["dir"] != "ltr" {
		t.Errorf("html attributes without a locale: got %v, want en, ltr", got)
	}
}

func TestStatusRows(t *testing.T) {
	ctx := locale(t)
	for name, tc := range map[string]struct {
		st   Status
		want string
	}{
		"revision":    {Status{Version: "v1", Revision: "abc123"}, "abc123"},
		"no revision": {Status{Version: "v1"}, "Not recorded"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := statusRows(tc.st)[3].Value.Render(ctx, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("revision row: got %q, want %q in it", out.String(), tc.want)
			}
		})
	}
}

// TestLinksSanitised: a link templ cannot trust is replaced.
func TestLinksSanitised(t *testing.T) {
	if got := link("javascript:alert(1)", "")["href"]; got != "about:invalid#TemplFailedSanitizationURL" {
		t.Errorf("href: got %v, want templ's replacement", got)
	}
}

// TestEscaped: values are escaped, never markup.
func TestEscaped(t *testing.T) {
	var out bytes.Buffer
	if err := Value(`<script>alert("x")</script>`).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "<script>") {
		t.Errorf("got %q, want it escaped", got)
	}
}
