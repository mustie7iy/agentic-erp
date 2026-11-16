package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestWalkthroughToleratedStatus: a command substitution in the
// walkthrough whose last command may end with status 1 as an answer, not
// a failure, says so inside it, $(grep -c x f || true) (stop_unseen in
// scripts/walkthrough.sh). Batch 75 passed `make walkthrough` on macOS's
// bash 3.2, which runs a lone simple command in $(...) without the ERR
// trap, and stopped on GitHub's bash 5 when its $(grep -c …) counted none
// (ci:batch75). Held: grep -c anywhere in a substitution (a count of 0 is
// an answer, which the caller's check judges), and grep, diff or cmp as
// the substitution's only command (no match, or a difference, is an
// answer too). A substitution whose failure is a real failure keeps it
// in a pipeline or a list, where every bash runs the ERR trap.
func TestWalkthroughToleratedStatus(t *testing.T) {
	files, err := filepath.Glob("../../scripts/walkthrough/*.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append([]string{"../../scripts/walkthrough.sh"}, files...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		subs := shellSubstitutions(string(b))
		if want := plainSubstitutions(string(b)); len(subs) != want {
			t.Errorf("%s: command substitutions read: got %d, want %d (the $( on lines outside comments and heredocs): the reader lost its place, or a $( sits in single quotes, a comment after code or $($(…))", f, len(subs), want)
		}
		for _, s := range subs {
			if why := untolerated(s.segments); why != "" {
				t.Errorf("%s:%d: $(%s): got %s without || true inside the substitution, want $(… || true) (stop_unseen)", f, s.line, s.body, why)
			}
		}
	}
}

// TestUntolerated holds the rule TestWalkthroughToleratedStatus applies,
// and the reading of substitutions it rests on: each finding names the
// substitution, so one read to the wrong end shows.
func TestUntolerated(t *testing.T) {
	for name, c := range map[string]struct {
		script string
		want   []string
	}{
		"a count assigned":                {`x=$(grep -c a f)`, []string{"grep -c: grep -c a f"}},
		"a count tolerated":               {`x=$(grep -c a f || true)`, nil},
		"a count tolerated by :":          {`x=$(grep -c a f || :)`, nil},
		"a count at a pipeline's end":     {`expect n "$(printf x | grep -cF a)" 1`, []string{"grep -c: printf x | grep -cF a"}},
		"a cluster of flags":              {`expect n "$(grep -cvxFf a b)" 0`, []string{"grep -c: grep -cvxFf a b"}},
		"a long flag":                     {`expect n "$(grep --count a b)" 0`, []string{"grep -c: grep --count a b"}},
		"a lone grep":                     {`grant r $(grep '\.approve$' f)`, []string{`a lone grep: grep '\.approve$' f`}},
		"a lone grep redirected":          {`say "$(grep a f 2>&1)"`, []string{"a lone grep: grep a f 2>&1"}},
		"a lone grep with a variable":     {`say "$(LC_ALL=C command grep a f)"`, []string{"a lone grep: LC_ALL=C command grep a f"}},
		"a lone diff":                     {`say "$(diff a b)"`, []string{"a lone diff: diff a b"}},
		"a lone cmp":                      {`say "$(cmp -s a b)"`, []string{"a lone cmp: cmp -s a b"}},
		"a grep feeding a pipeline":       {`x=$(grep a f | sed 's/a/b/')`, nil},
		"a grep in a list":                {`x=$(cd d && grep -l a f)`, nil},
		"another command":                 {`x=$(jq -r '.a | (.b)' f)`, nil},
		"nested in quotes":                {`say "$(f "$(grep -c a f)")"`, []string{"grep -c: grep -c a f"}},
		"a comment":                       {"# don't $(grep -c a f)\n: x # nor $(diff a b)", nil},
		"a heredoc":                       {"cat <<'YAML'\n$(grep -c a f) don't\nYAML\nx=$(diff a b)", []string{"a lone diff: diff a b"}},
		"arithmetic":                      {`n=$(( $(grep -c a f || true) + 1 ))`, nil},
		"a case inside a substitution":    {"x=$(case $y in a) grep a f ;; esac)\nz=$(grep -c a f)", []string{"grep -c: grep -c a f"}},
		"a case ended by its parenthesis": {"x=$(case $y in a) grep -c a f ;; esac)", []string{"grep -c: case $y in a) grep -c a f ;; esac"}},
		"a parenthesis in single quotes":  {`x=$(grep -c ')' f)`, []string{"grep -c: grep -c ')' f"}},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, s := range shellSubstitutions(c.script) {
				if why := untolerated(s.segments); why != "" {
					got = append(got, why+": "+s.body)
				}
			}
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("untolerated substitutions in %q: got %q, want %q", c.script, got, c.want)
			}
		})
	}
}

// TestWalkthroughRefusesOldBash: the walkthrough refuses a bash older
// than 5 before it does anything, naming the remedy. It runs where
// /bin/bash is that old (macOS, where the lanes and make ci-local run).
func TestWalkthroughRefusesOldBash(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "/bin/bash", "-c", `echo "${BASH_VERSINFO[0]}"`).Output()
	if err != nil {
		t.Skipf("no /bin/bash to run: %v", err)
	}
	if major, err := strconv.Atoi(strings.TrimSpace(string(out))); err != nil || major >= 5 {
		t.Skipf("/bin/bash is version %s: the refusal needs one older than 5", strings.TrimSpace(string(out)))
	}
	run := exec.CommandContext(t.Context(), "/bin/bash", "../../scripts/walkthrough.sh")
	run.Env = append(os.Environ(), "DATABASE_URL=postgres://127.0.0.1:1/none", "WALKTHROUGH_DIR=")
	got, err := run.CombinedOutput()
	if err == nil {
		t.Errorf("walkthrough under an old bash: got exit status 0, want a refusal\n%s", got)
	}
	for _, want := range []string{"needs bash 5 or later", "brew install bash"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("walkthrough under an old bash: got\n%s\nwant it to contain %q", got, want)
		}
	}
	if strings.Contains(string(got), "Step ") || strings.Contains(string(got), "go build") {
		t.Errorf("walkthrough under an old bash: got\n%s\nwant it to stop before any step or build", got)
	}
}

// shellSubst is a command substitution, $( … ), of a bash script: the
// line it starts on, its text and its commands at its own level.
type shellSubst struct {
	line     int
	body     string
	segments []shellSegment
}

// shellSegment is one command of a substitution's own level and the
// operator after it (|, ||, &&, ;, & or a newline; "" for the last).
type shellSegment struct {
	text, op string
}

// shellReader reads a bash script far enough to find its command
// substitutions: quotes, escapes, comments, heredocs and nesting, not
// the whole grammar.
type shellReader struct {
	src      string
	i        int
	heredocs []string
	found    []shellSubst
}

// shellSubstitutions are every command substitution of src, nested ones
// included, outside comments and heredoc bodies.
func shellSubstitutions(src string) []shellSubst {
	r := &shellReader{src: src}
	r.level(false)
	return r.found
}

// level reads commands until the end of src or, when closing, the ')'
// that ends the level, and returns the level's commands.
func (r *shellReader) level(closing bool) []shellSegment {
	var segs []shellSegment
	start, depth, caseDepth := r.i, 0, 0
	cut := func(op string, width int) {
		segs = append(segs, shellSegment{text: strings.TrimSpace(r.src[start:r.i]), op: op})
		r.i += width
		start = r.i
	}
	for r.i < len(r.src) {
		c := r.src[r.i]
		switch {
		case c == '\\':
			r.i += 2
		case c == '\'':
			r.skipPast('\'')
		case c == '"':
			r.double()
		case c == '#' && r.wordStart():
			for r.i < len(r.src) && r.src[r.i] != '\n' {
				r.i++
			}
		case strings.HasPrefix(r.src[r.i:], "$("):
			r.substitution()
		case strings.HasPrefix(r.src[r.i:], "<<") && !strings.HasPrefix(r.src[r.i:], "<<<"):
			r.heredoc()
		case r.word("case"):
			caseDepth++
			r.i += 4
		case r.word("esac"):
			caseDepth--
			r.i += 4
		case c == '(':
			depth++
			r.i++
		case c == ')' && depth > 0:
			depth--
			r.i++
		case c == ')' && caseDepth > 0:
			r.i++ // a case pattern's end
		case c == ')' && closing:
			cut("", 0)
			r.i++
			return segs
		case c == '\n':
			cut("\n", 1)
			r.skipHeredocs()
		case depth == 0 && strings.ContainsRune("|&;", rune(c)) && !strings.ContainsRune("<>", rune(r.prev())):
			op := r.src[r.i : r.i+1]
			if r.i+1 < len(r.src) && strings.ContainsRune("|&;", rune(r.src[r.i+1])) {
				op = r.src[r.i : r.i+2]
			}
			cut(op, len(op))
		default:
			r.i++
		}
	}
	cut("", 0)
	return segs
}

// substitution reads $( … ) or $(( … )) at r.i, recording the first.
func (r *shellReader) substitution() {
	at := r.i
	r.i += 2
	if r.i < len(r.src) && r.src[r.i] == '(' {
		r.level(true) // arithmetic: its own '(' and the closing '))'
		return
	}
	segs := r.level(true)
	r.found = append(r.found, shellSubst{
		line:     strings.Count(r.src[:at], "\n") + 1,
		body:     r.src[at+2 : r.i-1],
		segments: segs,
	})
}

// double reads a double-quoted word at r.i, with the substitutions in it.
func (r *shellReader) double() {
	r.i++
	for r.i < len(r.src) {
		switch {
		case r.src[r.i] == '\\':
			r.i += 2
		case r.src[r.i] == '"':
			r.i++
			return
		case strings.HasPrefix(r.src[r.i:], "$("):
			r.substitution()
		default:
			r.i++
		}
	}
}

// heredoc notes the delimiter of <<WORD, <<-WORD or <<'WORD' at r.i; its
// body starts on the next line.
func (r *shellReader) heredoc() {
	r.i += 2
	m := heredocWord.FindStringSubmatch(r.src[r.i:])
	if m == nil {
		return
	}
	r.heredocs = append(r.heredocs, m[1])
	r.i += len(m[0])
}

// skipHeredocs skips the bodies of the heredocs the line just ended
// opened, up to each one's delimiter line.
func (r *shellReader) skipHeredocs() {
	for _, delim := range r.heredocs {
		for r.i < len(r.src) {
			end := strings.IndexByte(r.src[r.i:], '\n')
			if end < 0 {
				end = len(r.src) - r.i
			}
			line := r.src[r.i : r.i+end]
			r.i = min(r.i+end+1, len(r.src))
			if strings.TrimSpace(line) == delim {
				break
			}
		}
	}
	r.heredocs = nil
}

// skipPast moves r.i past the next c after r.i.
func (r *shellReader) skipPast(c byte) {
	end := strings.IndexByte(r.src[r.i+1:], c)
	if end < 0 {
		r.i = len(r.src)
		return
	}
	r.i += end + 2
}

func (r *shellReader) prev() byte {
	if r.i == 0 {
		return '\n'
	}
	return r.src[r.i-1]
}

// wordStart: r.i begins a word (after a blank, an operator or a newline).
func (r *shellReader) wordStart() bool {
	return strings.ContainsRune(" \t\n;|&()", rune(r.prev()))
}

// word: the reserved word w stands at r.i as a word of its own.
func (r *shellReader) word(w string) bool {
	if !r.wordStart() || !strings.HasPrefix(r.src[r.i:], w) {
		return false
	}
	end := r.i + len(w)
	return end == len(r.src) || strings.ContainsRune(" \t\n;)&|", rune(r.src[end]))
}

// untolerated says why a substitution with these commands needs || true
// inside it, or "" when it does not: it tolerates a status other than 0
// already, or no command in it answers with one.
func untolerated(segs []shellSegment) string {
	if n := len(segs); n >= 2 && segs[n-2].op == "||" && (segs[n-1].text == "true" || segs[n-1].text == ":") {
		return ""
	}
	for _, s := range segs {
		if countsMatches(s.text) {
			return "grep -c"
		}
	}
	if len(segs) == 1 {
		switch name, _ := shellCommand(segs[0].text); name {
		case "grep", "egrep", "fgrep", "diff", "cmp":
			return "a lone " + name
		}
	}
	return ""
}

// shellCommand is the command a simple command runs and its arguments,
// past variable assignments and the builtin `command`.
func shellCommand(text string) (string, []string) {
	fields := strings.Fields(text)
	for len(fields) > 0 && (assignment.MatchString(fields[0]) || fields[0] == "command") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], fields[1:]
}

var (
	assignment  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	countFlags  = regexp.MustCompile(`^(-[A-Za-z]*c[A-Za-z]*|--count)$`)
	heredocWord = regexp.MustCompile(`^-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
	heredocLine = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_]+)`)
	plainSubst  = regexp.MustCompile(`(^|[^\\])\$\([^(]`) // not \$( , which prints $(
)

func countFlag(arg string) bool { return countFlags.MatchString(arg) }

// countsMatches: a grep with a count flag runs anywhere in the command,
// a branch of a case or an if included.
func countsMatches(text string) bool {
	fields := strings.Fields(text)
	for i, f := range fields {
		if (f == "grep" || f == "egrep" || f == "fgrep") && slices.ContainsFunc(fields[i+1:], countFlag) {
			return true
		}
	}
	return false
}

// plainSubstitutions counts the $( of src (not $(( or \$( ) on lines that are
// not comments and not in a heredoc: what shellSubstitutions must find
// when no line holds a comment after code.
func plainSubstitutions(src string) int {
	n, delim := 0, ""
	for line := range strings.Lines(src) {
		trimmed := strings.TrimSpace(line)
		switch {
		case delim != "":
			if trimmed == delim {
				delim = ""
			}
		case strings.HasPrefix(trimmed, "#"):
		default:
			n += len(plainSubst.FindAllString(line, -1))
			if m := heredocLine.FindStringSubmatch(line); m != nil && !strings.Contains(line, "<<<") {
				delim = m[1]
			}
		}
	}
	return n
}
