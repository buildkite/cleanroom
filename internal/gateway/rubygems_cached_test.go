package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubRubyGemsHandlerProvider struct {
	handler      http.Handler
	policyHost   string
	policyPort   int
	upstreamHost string
	upstreamPort int
	err          error
}

func (s *stubRubyGemsHandlerProvider) RubyGemsUpstream() (string, int, string, int, error) {
	if s.err != nil {
		return "", 0, "", 0, s.err
	}
	return s.policyHost, s.policyPort, s.upstreamHost, s.upstreamPort, nil
}

func (s *stubRubyGemsHandlerProvider) RubyGemsHandler() (http.Handler, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.handler, nil
}

func TestCachedRubyGemsHandlerStripsPrefixAndDelegates(t *testing.T) {
	t.Parallel()

	var capturedPath string
	provider := &stubRubyGemsHandlerProvider{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}),
		policyHost:   "rubygems.org",
		policyPort:   443,
		upstreamHost: "rubygems.org",
		upstreamPort: 443,
	}
	h := newCachedRubyGemsHandler(provider, nil)

	req := httptest.NewRequest(http.MethodGet, "/rubygems/versions", nil)
	req = withScope(req, registryTestScope("rubygems.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/versions"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestCachedRubyGemsHandlerDeniesUnallowedHost(t *testing.T) {
	t.Parallel()

	provider := &stubRubyGemsHandlerProvider{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("handler should not run for denied host")
		}),
		policyHost:   "rubygems.org",
		policyPort:   443,
		upstreamHost: "rubygems.org",
		upstreamPort: 443,
	}
	h := newCachedRubyGemsHandler(provider, nil)

	req := httptest.NewRequest(http.MethodGet, "/rubygems/versions", nil)
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

func TestCachedRubyGemsHandlerRejectsPost(t *testing.T) {
	t.Parallel()

	h := newCachedRubyGemsHandler(&stubRubyGemsHandlerProvider{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("handler should not run for POST")
		}),
		policyHost:   "rubygems.org",
		policyPort:   443,
		upstreamHost: "rubygems.org",
		upstreamPort: 443,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/rubygems/versions", nil)
	req = withScope(req, registryTestScope("rubygems.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestCachedRubyGemsHandlerNoScope(t *testing.T) {
	t.Parallel()

	h := newCachedRubyGemsHandler(&stubRubyGemsHandlerProvider{
		handler:      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		policyHost:   "rubygems.org",
		policyPort:   443,
		upstreamHost: "rubygems.org",
		upstreamPort: 443,
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/rubygems/versions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCachedRubyGemsHandlerUnavailable(t *testing.T) {
	t.Parallel()

	h := newCachedRubyGemsHandler(&stubRubyGemsHandlerProvider{
		err: errors.New("unavailable"),
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/rubygems/versions", nil)
	req = withScope(req, registryTestScope("rubygems.org"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
}
