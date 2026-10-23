package menubar

import (
	"strings"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/uiport"
)

// Routes the web page's to= takes, and the only ones the app opens: the
// overview (""), requests, proposals, requests/ENV and env/ENV, ENV a
// valid environment name.
func validRoute(route string) bool {
	switch route {
	case "", daemonproto.RouteRequests, daemonproto.RouteProposals:
		return true
	}
	for _, p := range []string{"env/", "requests/"} {
		if env, ok := strings.CutPrefix(route, p); ok {
			return daemonproto.ValidEnv(env)
		}
	}
	return false
}

// pageURL is what the app opens for route: the daemon's one-time code
// URL, and &to=ROUTE unless the route is the overview. ok is false, and
// nothing is to be opened, unless the daemon serves the page at the web
// UI's own address for this build (uiport: a checkout's, dev, or a
// release's), has a code URL, and the route is one of the page's. The
// code goes to no other address, whatever the daemon says.
func pageURL(w *daemonproto.WebResult, route string, dev bool) (u string, ok bool) {
	if w == nil || !w.Listening || w.URL != uiport.Origin(dev)+"/" || w.Code() == "" || !validRoute(route) {
		return "", false
	}
	if route == "" {
		return w.CodeURL, true
	}
	return w.CodeURL + "&to=" + route, true
}
