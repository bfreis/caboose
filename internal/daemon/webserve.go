package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/webui"
)

// defaultWebRetry is how often the daemon tries again to serve its page
// when it could not (the port in use): never on another port.
const defaultWebRetry = 30 * time.Second

// webShutdown is how long the page's server has, when the daemon stops,
// for the requests it is serving.
const webShutdown = 2 * time.Second

// webDispatch is the page's way into the method table.
type webDispatch struct{ s *server }

// Allowed reports whether the table lets the web page call the method.
func (d webDispatch) Allowed(name string) bool {
	m := d.s.lookup(name)
	return m != nil && slices.Contains(m.clients, daemonproto.ClientWeb)
}

// Call runs it as the page's, through callWeb.
func (d webDispatch) Call(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, *daemonproto.Error) {
	return d.s.callWeb(ctx, name, params)
}

// webActivity is an authenticated request of the page's: client activity,
// for the idle exit, while a page polls.
func (s *server) webActivity() {
	s.mu.Lock()
	s.webActive = time.Now()
	s.mu.Unlock()
}

// startWeb serves the page, when there is one to serve: in a goroutine of
// its own, which tries again while the address cannot be had, until the
// daemon stops (stopWeb). It runs at the start, and again when a rotation
// gives a daemon that started with no token one; it starts at most one
// server, and none once the daemon is stopping.
func (s *server) startWeb() {
	if s.web == nil || !s.web.servable() {
		if s.web != nil {
			s.log.Printf("web: the page is not served: %s", s.web.state().Error)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || s.webDone != nil {
		return
	}
	s.webDone = make(chan struct{})
	go s.serveWeb(s.webDone)
}

func (s *server) serveWeb(done chan struct{}) {
	defer close(done)
	retry := s.opts.webRetry
	if retry <= 0 {
		retry = defaultWebRetry
	}
	t := time.NewTicker(retry)
	defer t.Stop()
	for {
		if !s.serveWebOnce(retry) {
			return
		}
		select {
		case <-s.stopped:
			return
		case <-t.C:
		}
	}
}

// serveWebOnce listens and serves until the server ends, and reports
// whether to try again.
func (s *server) serveWebOnce(retry time.Duration) bool {
	s.mu.Lock()
	s.webListens++
	s.mu.Unlock()
	ln, err := webui.Listen(s.web.want)
	if err != nil {
		if s.web.setServed(false, s.web.want, err) {
			s.log.Printf("web: the page is not served: %v (trying again every %v)", err, retry)
		}
		return true
	}
	bound, _ := webui.Bound(ln)
	h := webui.Handler(webui.Options{
		Origin:   webui.Origin(bound),
		Token:    s.web.current,
		Redeem:   s.web.redeem,
		Dispatch: webDispatch{s},
		Activity: s.webActivity,
		Log:      s.log,
	})
	srv := webui.NewServer(h, webui.ErrorLog(s.log, nil))
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		ln.Close()
		return false
	}
	s.webSrv = srv
	s.mu.Unlock()
	if s.web.setServed(true, bound, nil) {
		s.log.Printf("web: serving the page on %s/", webui.Origin(bound))
	}
	err = srv.Serve(ln)
	s.mu.Lock()
	if s.webSrv == srv {
		s.webSrv = nil
	}
	stopping := s.stopping
	s.mu.Unlock()
	if stopping || errors.Is(err, http.ErrServerClosed) {
		return false
	}
	if s.web.setServed(false, s.web.want, err) {
		s.log.Printf("web: the page stopped being served: %v (trying again every %v)", err, retry)
	}
	return true
}

// stopWeb ends the page's server, giving the requests it serves a moment,
// and waits for it: before the links close, so no request of the page's
// reaches a link that is going.
func (s *server) stopWeb() {
	s.mu.Lock()
	done := s.webDone
	if done == nil {
		s.mu.Unlock()
		return
	}
	srv := s.webSrv
	s.webSrv = nil
	s.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), webShutdown)
		_ = srv.Shutdown(ctx)
		cancel()
		_ = srv.Close()
	}
	<-done
	s.web.setServed(false, s.web.want, errors.New("the daemon stopped"))
}
