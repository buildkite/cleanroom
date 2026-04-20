package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type stubGoProxyHandlerProvider struct {
	goHandler       http.Handler
	sumdbHandler    http.Handler
	goPolicyHost    string
	goPolicyPort    int
	goUpstreamHost  string
	goUpstreamPort  int
	sumPolicyHost   string
	sumPolicyPort   int
	sumUpstreamHost string
	sumUpstreamPort int
	err             error
}

func (s *stubGoProxyHandlerProvider) GoProxyUpstream() (string, int, string, int, error) {
	if s.err != nil {
		return "", 0, "", 0, s.err
	}
	return s.goPolicyHost, s.goPolicyPort, s.goUpstreamHost, s.goUpstreamPort, nil
}

func (s *stubGoProxyHandlerProvider) GoProxyHandlerForPolicy(*policy.CompiledPolicy) (http.Handler, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.goHandler, nil
}

func (s *stubGoProxyHandlerProvider) SumDBUpstream() (string, int, string, int, error) {
	if s.err != nil {
		return "", 0, "", 0, s.err
	}
	return s.sumPolicyHost, s.sumPolicyPort, s.sumUpstreamHost, s.sumUpstreamPort, nil
}

func (s *stubGoProxyHandlerProvider) SumDBHandler() (http.Handler, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.sumdbHandler, nil
}

func TestCachedGoProxyHandlerStripsPrefixAndDelegates(t *testing.T) {
	t.Parallel()

	var capturedPath string
	h := newCachedGoProxyHandler(&stubGoProxyHandlerProvider{
		goHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}),
		goPolicyHost:    "proxy.golang.org",
		goPolicyPort:    443,
		goUpstreamHost:  "proxy.golang.org",
		goUpstreamPort:  443,
		sumPolicyHost:   "sum.golang.org",
		sumPolicyPort:   443,
		sumUpstreamHost: "sum.golang.org",
		sumUpstreamPort: 443,
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/pkg/errors/@v/list", nil)
	req = withScope(req, registryTestScope("proxy.golang.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/github.com/pkg/errors/@v/list"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestCachedGoProxyHandlerDelegatesSumDBPath(t *testing.T) {
	t.Parallel()

	var capturedPath string
	h := newCachedGoProxyHandler(&stubGoProxyHandlerProvider{
		goHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("goproxy handler should not handle sumdb path")
		}),
		sumdbHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}),
		goPolicyHost:    "proxy.golang.org",
		goPolicyPort:    443,
		goUpstreamHost:  "proxy.golang.org",
		goUpstreamPort:  443,
		sumPolicyHost:   "sum.golang.org",
		sumPolicyPort:   443,
		sumUpstreamHost: "sum.golang.org",
		sumUpstreamPort: 443,
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/goproxy/sumdb/sum.golang.org/supported", nil)
	req = withScope(req, registryTestScope("sum.golang.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/sumdb/sum.golang.org/supported"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestCachedGoProxyHandlerDeniesUnallowedHost(t *testing.T) {
	t.Parallel()

	h := newCachedGoProxyHandler(&stubGoProxyHandlerProvider{
		goHandler:       http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler should not run") }),
		sumdbHandler:    http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler should not run") }),
		goPolicyHost:    "proxy.golang.org",
		goPolicyPort:    443,
		goUpstreamHost:  "proxy.golang.org",
		goUpstreamPort:  443,
		sumPolicyHost:   "sum.golang.org",
		sumPolicyPort:   443,
		sumUpstreamHost: "sum.golang.org",
		sumUpstreamPort: 443,
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/pkg/errors/@v/list", nil)
	req = withScope(req, registryTestScope("github.com"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonHostNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonHostNotAllowed, got)
	}
}

func TestCachedGoProxyHandlerUnavailable(t *testing.T) {
	t.Parallel()

	h := newCachedGoProxyHandler(&stubGoProxyHandlerProvider{err: errors.New("unavailable")}, nil)

	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/pkg/errors/@v/list", nil)
	req = withScope(req, registryTestScope("proxy.golang.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
}

func TestCachedGoProxyHandlerRejectsHead(t *testing.T) {
	t.Parallel()

	h := newCachedGoProxyHandler(&stubGoProxyHandlerProvider{
		goHandler:       http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler should not run") }),
		sumdbHandler:    http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler should not run") }),
		goPolicyHost:    "proxy.golang.org",
		goPolicyPort:    443,
		goUpstreamHost:  "proxy.golang.org",
		goUpstreamPort:  443,
		sumPolicyHost:   "sum.golang.org",
		sumPolicyPort:   443,
		sumUpstreamHost: "sum.golang.org",
		sumUpstreamPort: 443,
	}, nil)

	req := httptest.NewRequest(http.MethodHead, "/goproxy/github.com/pkg/errors/@v/list", nil)
	req = withScope(req, registryTestScope("proxy.golang.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonMethodNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonMethodNotAllowed, got)
	}
}
