package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewContentCacheHTTPClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	redirected := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()

	client := newContentCacheHTTPClient(nil)
	resp, err := client.Get(redirectSource.URL)
	if err != nil {
		t.Fatalf("get redirected source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected %d, got %d", http.StatusFound, resp.StatusCode)
	}
	if redirected {
		t.Fatal("expected redirected target to remain unrequested")
	}
}
