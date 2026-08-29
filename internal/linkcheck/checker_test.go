package linkcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testChecker() *Checker { return New("test-agent", 5*time.Second) }

// The basic contract: a live page is fine, a dead one is reported.
func TestCheckReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/gone":
			w.WriteHeader(http.StatusNotFound)
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	cases := map[string]int{"/ok": 200, "/gone": 404, "/broken": 500}

	for path, want := range cases {
		got := testChecker().Check(context.Background(), srv.URL+path)
		if got.Error != "" {
			t.Errorf("%s: unexpected error %q", path, got.Error)
		}
		if got.StatusCode != want {
			t.Errorf("%s: status = %d, want %d", path, got.StatusCode, want)
		}
	}
}

// HEAD is tried first so we do not pull bodies off other people's servers.
func TestCheckPrefersHEAD(t *testing.T) {
	var methods []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	testChecker().Check(context.Background(), srv.URL+"/page")

	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Errorf("methods used = %v, want a single HEAD", methods)
	}
}

// Plenty of servers reject HEAD while serving GET perfectly well. Reporting
// those as broken would be a false alarm on a working link, which is worse
// than the extra request.
func TestCheckFallsBackToGETWhenHEADIsRejected(t *testing.T) {
	for _, headStatus := range []int{405, 403, 400, 501} {
		var methods []string

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			methods = append(methods, r.Method)
			if r.Method == http.MethodHead {
				w.WriteHeader(headStatus)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))

		got := testChecker().Check(context.Background(), srv.URL+"/page")
		srv.Close()

		if len(methods) != 2 || methods[1] != http.MethodGet {
			t.Errorf("HEAD %d: methods = %v, want HEAD then GET", headStatus, methods)
		}
		if got.StatusCode != 200 {
			t.Errorf("HEAD %d: final status = %d, want 200 from the GET", headStatus, got.StatusCode)
		}
	}
}

// A genuine 404 must not trigger the fallback, or every dead link costs two
// requests to confirm.
func TestCheckDoesNotFallBackOnRealFailure(t *testing.T) {
	var methods []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	testChecker().Check(context.Background(), srv.URL+"/gone")

	if len(methods) != 1 {
		t.Errorf("methods = %v, want a single request", methods)
	}
}

// A link that redirects still gets the visitor somewhere, so it is not broken.
func TestCheckFollowsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := testChecker().Check(context.Background(), srv.URL+"/old")

	if got.StatusCode != 200 || got.Error != "" {
		t.Errorf("got status %d error %q, want 200 and no error", got.StatusCode, got.Error)
	}
}

// A redirect loop is a broken link, and must not hang the checker.
func TestCheckDetectsRedirectLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	got := testChecker().Check(context.Background(), srv.URL+"/loop")

	if got.Error != "redirect loop" {
		t.Errorf("error = %q, want %q", got.Error, "redirect loop")
	}
}

// Transport failures have to read as something a site owner can act on: the
// raw Go error names the dialer and port, which is noise in a report.
func TestCheckDescribesFailuresPlainly(t *testing.T) {
	got := testChecker().Check(context.Background(), "https://this-domain-does-not-exist-xyz123.invalid/")

	if got.Error != "domain does not resolve" {
		t.Errorf("error = %q, want %q", got.Error, "domain does not resolve")
	}
}

func TestCheckHonoursCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if got := testChecker().Check(ctx, srv.URL+"/slow"); got.Error == "" {
		t.Error("a cancelled check reported no error")
	}
}

// Results must line up with their inputs, or every finding names the wrong URL.
func TestCheckAllPreservesOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	urls := []string{srv.URL + "/a", srv.URL + "/gone", srv.URL + "/c"}
	results := testChecker().CheckAll(context.Background(), urls, 2)

	if len(results) != len(urls) {
		t.Fatalf("got %d results, want %d", len(results), len(urls))
	}
	for i, r := range results {
		if r.URL != urls[i] {
			t.Errorf("result %d is for %q, want %q", i, r.URL, urls[i])
		}
	}
	if results[1].StatusCode != 404 {
		t.Errorf("the dead link reported %d, want 404", results[1].StatusCode)
	}
}

// These are other people's servers: the whole point of the cap is that we
// never arrive as a burst.
func TestCheckAllRespectsConcurrencyLimit(t *testing.T) {
	var mu struct {
		current, peak int
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-lock
		mu.current++
		if mu.current > mu.peak {
			mu.peak = mu.current
		}
		lock <- struct{}{}

		time.Sleep(30 * time.Millisecond)

		<-lock
		mu.current--
		lock <- struct{}{}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	urls := make([]string, 12)
	for i := range urls {
		urls[i] = srv.URL + "/page"
	}

	testChecker().CheckAll(context.Background(), urls, 3)

	if mu.peak > 3 {
		t.Errorf("peak concurrency = %d, want at most 3", mu.peak)
	}
}
