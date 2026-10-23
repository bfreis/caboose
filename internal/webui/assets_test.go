package webui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// embedded is every file under static/, by name.
func embedded(t *testing.T) map[string][]byte {
	t.Helper()
	m := map[string][]byte{}
	err := fs.WalkDir(static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "static" {
				t.Errorf("a directory under static/: %s (the table serves no directories)", p)
			}
			return nil
		}
		b, err := static.ReadFile(p)
		m[strings.TrimPrefix(p, "static/")] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Every file under static/ is served, or a test's, and nothing a test's
// is served.
func TestEveryAssetServedOrATest(t *testing.T) {
	files := embedded(t)
	for name := range files {
		_, ok := served[name]
		switch {
		case name == indexName:
		case strings.HasSuffix(name, ".test.mjs"):
			if ok {
				t.Errorf("%s is a test, and served", name)
			}
		case !ok:
			t.Errorf("%s is neither served nor a test: add it to served, or remove it", name)
		}
	}
	for name, typ := range served {
		if _, ok := files[name]; !ok {
			t.Errorf("%s is served but not embedded", name)
		}
		if strings.HasSuffix(name, ".test.mjs") || name == indexName || strings.Contains(name, "/") {
			t.Errorf("%s may not be served under /static/", name)
		}
		want := map[string]string{".css": typeCSS, ".mjs": "text/javascript; charset=utf-8", ".js": "text/javascript; charset=utf-8"}[path.Ext(name)]
		if want == "" || typ != want {
			t.Errorf("%s served as %q", name, typ)
		}
	}
}

// What a page's script may never use: every way to turn a string into
// markup or code.
var forbiddenJS = []string{
	"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function",
	"DOMParser", "createContextualFragment", "javascript:", "srcdoc", "setHTMLUnsafe", "parseHTMLUnsafe",
	"document.cookie", "import(",
}

// stringTimer is setTimeout or setInterval given a string.
var stringTimer = regexp.MustCompile(`set(Timeout|Interval)\s*\(\s*["'` + "`" + `]`)

// The page keeps its token in the tab's sessionStorage, never in
// localStorage, which whatever answers on the page's origin while the
// daemon is down could read back.
func TestAssetsKeepTheTokenInTheTab(t *testing.T) {
	files := embedded(t)
	for name, b := range files {
		if strings.HasSuffix(name, ".mjs") && !strings.HasSuffix(name, ".test.mjs") && strings.Contains(string(b), "localStorage") {
			t.Errorf("%s uses localStorage", name)
		}
	}
	if !strings.Contains(string(files["app.mjs"]), "storage: () => w.sessionStorage") {
		t.Error("app.mjs does not keep the token in sessionStorage")
	}
}

func TestAssetsUseNoHTMLSinks(t *testing.T) {
	for name, b := range embedded(t) {
		if !strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".test.mjs") {
			continue
		}
		s := string(b)
		for _, f := range forbiddenJS {
			if strings.Contains(s, f) {
				t.Errorf("%s uses %s", name, f)
			}
		}
		if stringTimer.MatchString(s) {
			t.Errorf("%s passes a string to a timer", name)
		}
	}
}

var (
	scriptTag   = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	styleAttr   = regexp.MustCompile(`(?i)\sstyle\s*=`)
	handlerAttr = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	styleTag    = regexp.MustCompile(`(?i)<style\b`)
	metaCSP     = regexp.MustCompile(`(?i)<meta[^>]+http-equiv\s*=\s*["']?content-security-policy`)
	htmlLang    = regexp.MustCompile(`(?i)<html\s[^>]*\blang\s*=\s*"en"`)
	control     = regexp.MustCompile(`(?is)<(input|select|textarea)\b[^>]*>`)
	idAttr      = regexp.MustCompile(`(?i)\sid\s*=\s*"([^"]*)"`)
	ariaLabel   = regexp.MustCompile(`(?i)\saria-label(ledby)?\s*=`)
	hiddenInput = regexp.MustCompile(`(?i)\stype\s*=\s*"hidden"`)
)

func TestIndexHTML(t *testing.T) {
	s := string(indexAsset.body)
	for _, m := range scriptTag.FindAllStringSubmatch(s, -1) {
		if strings.TrimSpace(m[2]) != "" {
			t.Errorf("an inline script: %q", m[0])
		}
		if !strings.Contains(m[1], `type="module"`) || !regexp.MustCompile(`src="/static/[a-z]+\.mjs"`).MatchString(m[1]) {
			t.Errorf("a script that is not one of the page's modules: %q", m[0])
		}
	}
	if styleAttr.MatchString(s) || styleTag.MatchString(s) {
		t.Error("inline style in index.html")
	}
	if handlerAttr.MatchString(s) {
		t.Error("an on* attribute in index.html")
	}
	if metaCSP.MatchString(s) {
		t.Error("a <meta> CSP: the header is the one source")
	}
	if !htmlLang.MatchString(s) {
		t.Error(`no lang="en" on <html>`)
	}
	for _, c := range control.FindAllString(s, -1) {
		if hiddenInput.MatchString(c) || ariaLabel.MatchString(c) {
			continue
		}
		id := idAttr.FindStringSubmatch(c)
		if id == nil || !strings.Contains(s, `for="`+id[1]+`"`) {
			t.Errorf("a control with no label: %s", c)
		}
	}
}

// No asset names a URL outside the page: no external script, style,
// image or connection, whatever the CSP would say.
var externalURL = regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.\[]`)

func TestAssetsNameNoExternalURL(t *testing.T) {
	for name, b := range embedded(t) {
		if strings.HasSuffix(name, ".test.mjs") {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := stripComment(name, line)
			for _, m := range externalURL.FindAllStringIndex(code, -1) {
				u := code[m[0]:]
				if strings.HasPrefix(u, "http://127.0.0.1:") || strings.HasPrefix(u, "http://localhost:") {
					continue // the page's own address, or a forwarded port's, in text
				}
				t.Errorf("%s:%d names an outside URL: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// stripComment is a line without a script's // comment (one that starts
// the line or follows a space) or CSS's /* */ comment on it.
func stripComment(name, line string) string {
	switch path.Ext(name) {
	case ".mjs", ".js":
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "*") || strings.HasPrefix(trim, "/*") {
			return ""
		}
		if i := strings.Index(line, " // "); i >= 0 {
			return line[:i]
		}
	case ".css":
		for {
			i := strings.Index(line, "/*")
			if i < 0 {
				break
			}
			j := strings.Index(line[i:], "*/")
			if j < 0 {
				return line[:i]
			}
			line = line[:i] + line[i+j+2:]
		}
	}
	return line
}

// The scans catch what they are for.
func TestAssetScansCatch(t *testing.T) {
	if !externalURL.MatchString(`fetch("https://evil.example/x")`) || !externalURL.MatchString(`src="//cdn.example/x.js"`) ||
		!externalURL.MatchString(`url(http://x.example/a.png)`) {
		t.Error("externalURL")
	}
	if stripComment("a.mjs", `  // see https://example.com`) != "" || stripComment("a.mjs", `x(); // https://example.com`) != "x();" {
		t.Error("stripComment")
	}
	if !stringTimer.MatchString(`setTimeout("alert(1)", 1)`) || stringTimer.MatchString(`setTimeout(() => f(), 1)`) {
		t.Error("stringTimer")
	}
	if !handlerAttr.MatchString(`<img src=x onerror=alert(1)>`) || !styleAttr.MatchString(`<p style="x">`) {
		t.Error("attributes")
	}
	if m := scriptTag.FindStringSubmatch(`<script>alert(1)</script>`); m == nil || strings.TrimSpace(m[2]) == "" {
		t.Error("scriptTag")
	}
}

// A narrow window stacks the tables' cells, each labelled with its
// column's head, so no page scrolls sideways (pages.mjs's dataTable gives
// the cells their data-label and the parts their roles).
func TestCSSStacksTablesWhenNarrow(t *testing.T) {
	css := string(embedded(t)["app.css"])
	i := strings.Index(css, "@media (max-width: 40rem) {")
	if i < 0 {
		t.Fatal("app.css has no narrow-window rules")
	}
	block := css[i:]
	if j := strings.Index(block, "\n}\n"); j >= 0 {
		block = block[:j]
	}
	for _, want := range []string{"tr,\n  th,\n  td {\n    display: block;", "thead tr {", "clip-path: inset(50%);", "overflow-wrap: anywhere;", "content: attr(data-label)"} {
		if !strings.Contains(block, want) {
			t.Errorf("the narrow-window rules lack %q", want)
		}
	}
	if strings.Contains(block, "display: none") {
		t.Error("the narrow-window rules hide something from assistive technology")
	}
}
