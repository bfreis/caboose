package webui

import (
	"embed"
	"fmt"
	"path"
)

// static is the page's files. Only what the tables below name is served:
// a file that is in neither is a test's (*.test.mjs) or a mistake, which
// a test refuses.
//
//go:embed static
var static embed.FS

// asset is one served file.
type asset struct {
	name string
	typ  string
	body []byte
}

// indexName is the page itself, served at /.
const indexName = "index.html"

// served is every file under /static/, by name, with its content type.
// Nothing else is served from there, whatever the embedded files hold.
var served = map[string]string{
	"app.css":   typeCSS,
	"app.mjs":   typeJS,
	"api.mjs":   typeJS,
	"dom.mjs":   typeJS,
	"model.mjs": typeJS,
	"pages.mjs": typeJS,
}

var (
	indexAsset = load(indexName, typeHTML)
	assets     = loadAll()
)

func load(name, typ string) asset {
	b, err := static.ReadFile(path.Join("static", name))
	if err != nil {
		panic(fmt.Sprintf("webui: the embedded %s: %v", name, err))
	}
	return asset{name: name, typ: typ, body: b}
}

func loadAll() map[string]asset {
	m := map[string]asset{}
	for name, typ := range served {
		m[name] = load(name, typ)
	}
	return m
}
