package launcher

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// waitFor polls cond until it holds, failing the test after a generous
// while: the condition is a goroutine reaching a point, never a timing.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// N calls while a run is under way share it: the work runs once, every
// caller gets its answer, a caller whose ctx ends returns at once without
// ending the run, and a call after the run ended runs afresh.
func TestFlightsShareOneRun(t *testing.T) {
	var f flights[int]
	var runs atomic.Int32
	release := make(chan struct{})
	work := func() (int, error) {
		n := runs.Add(1)
		<-release
		return int(n), nil
	}
	const n = 20
	var wg sync.WaitGroup
	got := make([]int, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = f.do(context.Background(), "alpha", work)
		}()
	}
	waitFor(t, "every call joins the run", func() bool { return f.callers("alpha") == n })

	// One more caller gives up while the run goes on.
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := f.do(ctx, "alpha", work)
		gaveUp <- err
	}()
	waitFor(t, "the last call joins the run", func() bool { return f.callers("alpha") == n+1 })
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Errorf("the caller that gave up: %v", err)
	}
	// Another key runs on its own.
	if v, err := f.do(context.Background(), "beta", func() (int, error) { return 7, nil }); v != 7 || err != nil {
		t.Errorf("beta: %d, %v", v, err)
	}

	close(release)
	wg.Wait()
	if r := runs.Load(); r != 1 {
		t.Fatalf("%d runs for %d concurrent calls", r, n)
	}
	for i := range n {
		if got[i] != 1 || errs[i] != nil {
			t.Errorf("call %d: %d, %v", i, got[i], errs[i])
		}
	}
	if f.callers("alpha") != 0 {
		t.Error("the run is still recorded after it ended")
	}
	if v, err := f.do(context.Background(), "alpha", work); v != 2 || err != nil {
		t.Errorf("a later call: %d, %v (want a fresh run)", v, err)
	}
}

// A run that panics answers every caller with an error, and the next
// call runs afresh.
func TestFlightsPanic(t *testing.T) {
	var f flights[int]
	if _, err := f.do(context.Background(), "alpha", func() (int, error) { panic("boom") }); err == nil {
		t.Fatal("a panicking run answered no error")
	}
	if v, err := f.do(context.Background(), "alpha", func() (int, error) { return 3, nil }); v != 3 || err != nil {
		t.Errorf("after the panic: %d, %v", v, err)
	}
}

// env_details and doctor called many times at once for an environment
// do their work once: the environment's App is made once.
func TestDetailsAndDoctorSingleFlight(t *testing.T) {
	for _, method := range []string{daemonproto.MethodEnvDetails, daemonproto.MethodDoctor} {
		t.Run(method, func(t *testing.T) {
			home := t.TempDir()
			var apps atomic.Int32
			release := make(chan struct{})
			l := NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(home, "daemon"), CabooseHome: home,
				NewApp: func(env string) (*App, error) {
					apps.Add(1)
					<-release
					return nil, errors.New("no such environment here")
				}})
			t.Cleanup(l.Close)
			call := func() error {
				var err error
				if method == daemonproto.MethodDoctor {
					_, err = l.Doctor(context.Background(), "alpha")
				} else {
					_, err = l.EnvDetails(context.Background(), "alpha")
				}
				return err
			}
			callers := func() int {
				if method == daemonproto.MethodDoctor {
					return l.doctorFlights.callers("alpha")
				}
				return l.detailsFlights.callers("alpha")
			}
			const n = 10
			errs := make(chan error, n)
			for range n {
				go func() { errs <- call() }()
			}
			waitFor(t, "every call joins the run", func() bool { return callers() == n })
			close(release)
			for range n {
				if err := <-errs; err == nil {
					t.Error("a call answered no error")
				}
			}
			if a := apps.Load(); a != 1 {
				t.Fatalf("%d runs for %d concurrent calls", a, n)
			}
			if err := call(); err == nil || apps.Load() != 2 {
				t.Errorf("a later call: %v, %d runs (want a fresh one)", err, apps.Load())
			}
		})
	}
}
