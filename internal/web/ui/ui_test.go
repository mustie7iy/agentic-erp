package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
		"Home":            Home(signedIn(), HomePage{Companies: []string{"Acme GmbH"}}),
		"homeContent":     homeContent(HomePage{}),
		"SignOut":         SignOut("token"),
		"SignIn":          SignIn(shell(), SignInForm{Code: "acme", Return: "/"}),
		"signInContent":   signInContent(SignInForm{Code: "Acme", Error: i18n.SignInInvalid}),
		"Problem":         Problem(shell(), i18n.SignInFailedTitle, i18n.SignInFailedBody, "/login", i18n.SignInAgain),
		"problemContent":  problemContent(i18n.RefusedTitle, i18n.RefusedBody, "/", i18n.NotFoundHome),
		"Continue":        Continue(continuing()),
		"continueContent": continueContent("/"),
		"StatusPage":      StatusPage(shell(), Status{Version: "v1.2.3", Revision: "abc"}),
		"statusContent":   statusContent(Status{Version: "devel"}),
		"NotFound":        NotFound(shell()),
		"notFoundContent": notFoundContent(),
		"Inbox":           Inbox(signedIn(), inboxFull()),
		"inbox empty":     inboxContent(InboxPage{}),
		"inbox refused":   inboxContent(InboxPage{Refusal: &Refusal{Title: i18n.InboxRefusedTitle, Message: i18n.ProblemPermission, Code: "identity.permission_missing"}}),
		"inbox last page": inboxContent(InboxPage{Approvals: []Approval{approval(false)}}),
		"Card":            Card(approval(true)),
		"card plain":      Card(Approval{Path: "/approvals/2", Title: "t", Kind: Label{Raw: "new_kind"}}),
		"Fact":            Fact(i18n.ApprovalKind, Value("v")),
		"Amount":          Amount(Money{Amount: "1.00", Currency: "EUR"}),
		"Principal":       Principal(Name{Text: "Avery"}),
		"principal agent": Principal(Name{Text: "Matcher", Agent: true}),
		"principal none":  Principal(Name{}),
		"Date":            Date(day),
		"StatusTag":       StatusTag(Label{Key: i18n.StatusApproved}),
		"status raw":      StatusTag(Label{Raw: "on_hold"}),
		"Barred":          Barred(i18n.InboxBarredBody, []string{"a.b", "c.d"}),
		"RefusalNotice":   RefusalNotice(refusal()),
		"refusal other":   RefusalNotice(Refusal{Title: i18n.RefusalTitle, Fallback: "Gone", Code: "x.y"}),
		"Request":         Request(signedIn(), requestPage(true)),
		"request closed":  requestContent(requestPage(false)),
		"StepItem":        StepItem(Step{No: 1, Required: 2, Approvals: 1, Current: true}),
		"step done":       StepItem(Step{No: 1, Required: 1, Approvals: 1}),
		"DecisionItem":    DecisionItem(Decision{Label: Label{Key: i18n.StatusRejected}, By: Name{Text: "B"}, At: day, Comment: "No"}),
		"decision bare":   DecisionItem(Decision{Label: Label{Key: i18n.StatusApproved}, By: Name{Text: "B"}, At: day}),
		"DecideForm":      DecideForm(forms(), true),
		"decide reject":   DecideForm(forms(), false),
		"decideHidden":    decideHidden("c", `"1"`, "k"),
		"CommentField":    CommentField("f", i18n.DecideComment, i18n.DecideCommentHint, "text", i18n.DecideTooLong),
		"comment plain":   CommentField("f", i18n.DecideComment, i18n.DecideCommentHint, "", ""),
	}
}

var day = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)

// approval is a request, an agent's when agent is true.
func approval(agent bool) Approval {
	return Approval{Path: "/approvals/1", Title: "Requisition PR1", Kind: Label{Key: i18n.ObjectRequisition}, Status: Label{Key: i18n.StatusPending},
		Amount: &Money{Amount: "12000.00", Currency: "EUR"}, Requester: Name{Text: "Avery", Agent: agent}, Requested: day, Step: 1, StepCount: 2,
		ObjectID: "0192"}
}

func inboxFull() InboxPage {
	return InboxPage{Approvals: []Approval{approval(true), approval(false)}, More: "/approvals?page=t", Done: i18n.InboxDoneApproved,
		Barred: []string{"workflow.request.decide"}}
}

func refusal() Refusal {
	return Refusal{Title: i18n.RefusalTitle, Message: i18n.ProblemSoD, Code: "identity.sod_conflict", Trace: "abc", Barred: []string{"a.b"}}
}

func forms() DecideForms {
	return DecideForms{Action: "/approvals/1", CSRF: "c", ETag: `"1"`, ApproveKey: "k1", RejectKey: "k2", Comment: "ok", ReasonError: i18n.DecideReasonNeeded}
}

// requestPage is a request's page: open to decide, with a refusal and
// decisions, or decided, with none.
func requestPage(open bool) RequestPage {
	if !open {
		return RequestPage{Approval: approval(false), Why: i18n.DecideClosed}
	}
	r := refusal()
	return RequestPage{Approval: approval(true), Steps: []Step{{No: 1, Required: 1, Approvals: 0, Current: true}},
		Decisions: []Decision{{Label: Label{Key: i18n.StatusApproved}, By: Name{Text: "B"}, At: day}}, Decide: new(forms()), Refusal: &r}
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

// signedIn is a signed-in person's shell.
func signedIn() Shell {
	s := shell()
	s.Person = &Person{CSRF: "token-1"}
	return s
}

// continuing is a continue page's shell.
func continuing() Shell {
	s := shell()
	s.Continue = "/health?x=1"
	return s
}

// TestSignedInLayout: a signed-in person's page has the sign-out form
// with the CSRF token, which htmx also sends as a header; a continue page
// goes on to its path at once; a page before sign-in has neither.
func TestSignedInLayout(t *testing.T) {
	ctx := locale(t)
	doc := parse(t, ctx, Home(signedIn(), HomePage{}))
	form := find(doc, "form")
	if got, _ := attr(form, "action"); got != "/logout" {
		t.Errorf("sign-out form: action %q", got)
	}
	if got, _ := attr(find(form, "input"), "value"); got != "token-1" {
		t.Errorf("sign-out form: token %q", got)
	}
	if got, _ := attr(find(doc, "body"), "hx-headers"); got != `{"X-CSRF-Token":"token-1"}` {
		t.Errorf("htmx headers: got %q", got)
	}
	cont := parse(t, ctx, Continue(continuing()))
	var refresh string
	walkAll(cont, func(n *html.Node) {
		if v, _ := attr(n, "http-equiv"); n.Data == "meta" && v == "refresh" {
			refresh, _ = attr(n, "content")
		}
	})
	if refresh != "0;url=/health?x=1" {
		t.Errorf("continue: refresh %q", refresh)
	}
	plain := parse(t, ctx, SignIn(shell(), SignInForm{}))
	if _, ok := attr(find(plain, "body"), "hx-headers"); ok {
		t.Error("before sign-in: htmx headers set")
	}
	walkAll(parse(t, ctx, SignIn(shell(), SignInForm{Code: "x", Error: i18n.SignInInvalid})), func(n *html.Node) {
		if id, _ := attr(n, "id"); id != "code" {
			return
		}
		invalid, _ := attr(n, "aria-invalid")
		described, _ := attr(n, "aria-describedby")
		if invalid != "true" || described != "code-hint code-error" {
			t.Errorf("the code with an error: aria-invalid %q, aria-describedby %q", invalid, described)
		}
	})
}

func walkAll(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkAll(c, fn)
	}
}

func TestLayout(t *testing.T) {
	doc := parse(t, locale(t), Home(shell(), HomePage{}))
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

// TestStatusKind: a status's tag colour.
func TestStatusKind(t *testing.T) {
	for k, want := range map[i18n.Key]string{i18n.StatusApproved: "success", i18n.StatusRejected: "inactive", i18n.StatusCancelled: "inactive",
		i18n.StatusPending: "", "": ""} {
		if got := statusKind(Label{Key: k}); got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
}

// TestAmountWithoutLocale: outside a request with a locale an amount is
// the API's.
func TestAmountWithoutLocale(t *testing.T) {
	var out bytes.Buffer
	if err := amount(Money{Amount: "1.00", Currency: "EUR"}).Render(context.Background(), &out); err != nil || out.String() != "1.00 EUR" {
		t.Errorf("got %q, %v", out.String(), err)
	}
}

// TestSignedInNav: the inbox joins the navigation of a person who may
// read approvals.
func TestSignedInNav(t *testing.T) {
	if got := SignedInNav(false); len(got) != 2 {
		t.Errorf("without: got %v", got)
	}
	if got := SignedInNav(true); len(got) != 3 || got[1].Path != ApprovalsPath {
		t.Errorf("with: got %v", got)
	}
	if got := navOf(Shell{Nav: SignedInNav(true)}); len(got) != 3 {
		t.Errorf("a shell's own: got %v", got)
	}
}

// TestDecideFormFields: a form's comment names its hint and error, and is
// marked invalid with one.
func TestDecideFormFields(t *testing.T) {
	doc := parse(t, locale(t), DecideForm(forms(), false))
	ta := find(doc, "textarea")
	for key, want := range map[string]string{"id": "reject-reason", "name": "comment", "aria-describedby": "reject-reason-hint reject-reason-error", "aria-invalid": "true"} {
		if got, _ := attr(ta, key); got != want {
			t.Errorf("textarea %s: got %q, want %q", key, got, want)
		}
	}
	if got, _ := attr(find(doc, "form"), "action"); got != "/approvals/1/reject" {
		t.Errorf("action: got %q", got)
	}
}
