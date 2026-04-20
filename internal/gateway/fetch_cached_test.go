package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubFetchHandlerProvider struct {
	handler      http.Handler
	allowedHosts map[string]struct{}
	err          error
}

func (s *stubFetchHandlerProvider) FetchAllowsHost(host string) bool {
	if s.allowedHosts == nil {
		return false
	}
	_, ok := s.allowedHosts[host]
	return ok
}

func (s *stubFetchHandlerProvider) FetchHandler() (http.Handler, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.handler, nil
}

func TestCachedFetchHandlerStripsPrefixAndDelegates(t *testing.T) {
	t.Parallel()

	var capturedPath string
	h := newCachedFetchHandler(&stubFetchHandlerProvider{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}),
		allowedHosts: map[string]struct{}{"dl.google.com": {}},
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/fetch/dl.google.com/go/go1.26.2.linux-amd64.tar.gz", nil)
	req = withScope(req, registryTestScope("dl.google.com"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/dl.google.com/go/go1.26.2.linux-amd64.tar.gz"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestCachedFetchHandlerDeniesHostOutsidePolicy(t *testing.T) {
	t.Parallel()

	h := newCachedFetchHandler(&stubFetchHandlerProvider{
		handler:      http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler should not run") }),
		allowedHosts: map[string]struct{}{"dl.google.com": {}},
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/fetch/dl.google.com/go/go1.26.2.linux-amd64.tar.gz", nil)
	req = withScope(req, registryTestScope("proxy.golang.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonHostNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonHostNotAllowed, got)
	}
}

func TestCachedFetchHandlerDeniesHostOutsideFetchAllowlist(t *testing.T) {
	t.Parallel()

	h := newCachedFetchHandler(&stubFetchHandlerProvider{
		handler:      http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler should not run") }),
		allowedHosts: map[string]struct{}{"objects.githubusercontent.com": {}},
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/fetch/dl.google.com/go/go1.26.2.linux-amd64.tar.gz", nil)
	req = withScope(req, registryTestScope("dl.google.com"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonHostNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonHostNotAllowed, got)
	}
}

func TestCachedFetchHandlerUnavailable(t *testing.T) {
	t.Parallel()

	h := newCachedFetchHandler(&stubFetchHandlerProvider{
		err:          errors.New("unavailable"),
		allowedHosts: map[string]struct{}{"dl.google.com": {}},
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/fetch/dl.google.com/go/go1.26.2.linux-amd64.tar.gz", nil)
	req = withScope(req, registryTestScope("dl.google.com"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
}

func TestCachedFetchHandlerRejectsInvalidPath(t *testing.T) {
	t.Parallel()

	h := newCachedFetchHandler(&stubFetchHandlerProvider{
		handler:      http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler should not run") }),
		allowedHosts: map[string]struct{}{"dl.google.com": {}},
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/fetch/dl.google.com", nil)
	req = withScope(req, registryTestScope("dl.google.com"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonInvalidRequest {
		t.Fatalf("expected reason %s, got %q", reasonInvalidRequest, got)
	}
}
