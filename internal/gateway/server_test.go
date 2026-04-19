package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestExtractSourceIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"10.1.1.2:43210", "10.1.1.2"},
		{"[::ffff:10.1.1.2]:43210", "10.1.1.2"},
		{"192.168.1.1:8080", "192.168.1.1"},
		{"[::1]:9090", "::1"},
		{"10.1.1.2", "10.1.1.2"},
	}
	for _, tt := range tests {
		got := extractSourceIP(tt.input)
		if got != tt.want {
			t.Errorf("extractSourceIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIdentityMiddleware403ForUnregisteredIP(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	srv := &Server{registry: reg}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo", nil)
	req.RemoteAddr = "10.99.99.99:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestIdentityMiddlewareInjectsScope(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.Register("10.1.1.2", "sandbox-1", p); err != nil {
		t.Fatalf("register: %v", err)
	}

	srv := &Server{registry: reg}
	var gotScope *SandboxScope
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotScope, _ = ScopeFromContext(r.Context())
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.1.1.2:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotScope == nil {
		t.Fatal("expected scope in context")
	}
	if gotScope.SandboxID != "sandbox-1" {
		t.Fatalf("expected sandbox-1, got %s", gotScope.SandboxID)
	}
}

func TestGatewayServiceForPathClassifiesRubyGems(t *testing.T) {
	t.Parallel()

	if got, want := gatewayServiceForPath("/rubygems/versions"), "rubygems"; got != want {
		t.Fatalf("unexpected service classification: got %q want %q", got, want)
	}
}

func TestTracingMiddlewareUsesActiveExecutionTrace(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.Register("10.1.1.2", "sandbox-1", p); err != nil {
		t.Fatalf("register: %v", err)
	}

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
	}()

	parentCtx, parentSpan := tracerProvider.Tracer("test").Start(context.Background(), "cleanroom.parent")
	reg.SetActiveExecutionTrace("sandbox-1", "exec-1", trace.SpanContextFromContext(parentCtx))

	srv := &Server{registry: reg, tracerProvider: tracerProvider}
	handler := srv.identityMiddleware(srv.pathMiddleware(srv.tracingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))))

	req := httptest.NewRequest(http.MethodGet, "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req.RemoteAddr = "10.1.1.2:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	parentSpan.End()

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	spans := recorder.Ended()
	var gatewaySpan sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == "cleanroom.gateway.git.request" {
			gatewaySpan = span
			break
		}
	}
	if gatewaySpan == nil {
		t.Fatalf("expected gateway span, got spans %#v", spans)
	}
	if got, want := gatewaySpan.SpanContext().TraceID(), parentSpan.SpanContext().TraceID(); got != want {
		t.Fatalf("unexpected gateway trace id: got %s want %s", got, want)
	}
	if got, want := gatewaySpan.Parent().SpanID(), parentSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected gateway parent span id: got %s want %s", got, want)
	}
}

func TestTracingMiddlewareEmitsGatewayMetrics(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny", Allow: []policy.AllowRule{{Host: "github.com"}}}
	if err := reg.Register("10.1.1.2", "sandbox-1", p); err != nil {
		t.Fatalf("register: %v", err)
	}

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() {
		_ = meterProvider.Shutdown(context.Background())
	}()

	srv := NewServer(ServerConfig{Registry: reg, MeterProvider: meterProvider})
	handler := srv.identityMiddleware(srv.pathMiddleware(srv.tracingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setGatewayRequestDecision(r.Context(), "allow", "proxied")
		w.WriteHeader(http.StatusNoContent)
	}))))

	req := httptest.NewRequest(http.MethodGet, "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req.RemoteAddr = "10.1.1.2:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	metrics := collectGatewayResourceMetrics(t, reader)
	requireGatewayMetricSum(t, metrics, "cleanroom_gateway_requests_total", map[string]string{
		"service":      "git",
		"action":       "allow",
		"reason_code":  "proxied",
		"status_class": "2xx",
	}, 1)
	requireGatewayHistogramCount(t, metrics, "cleanroom_gateway_request_duration_seconds", map[string]string{
		"service": "git",
		"action":  "allow",
	}, 1)
}

func TestGatewayStatusRecorderUnwrapsResponseWriterForResponseController(t *testing.T) {
	t.Parallel()

	underlying := httptest.NewRecorder()
	wrapped := &gatewayStatusRecorder{ResponseWriter: underlying}

	if err := http.NewResponseController(wrapped).Flush(); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}
	if !underlying.Flushed {
		t.Fatal("expected flush to reach underlying response writer")
	}
}

func collectGatewayResourceMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}
	return metrics
}

func requireGatewayMetricSum(t *testing.T, metrics metricdata.ResourceMetrics, name string, attrs map[string]string, want int64) {
	t.Helper()
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q had unexpected data type %T", name, metric.Data)
			}
			for _, point := range sum.DataPoints {
				if gatewayMetricAttributesMatch(point.Attributes, attrs) {
					if point.Value != want {
						t.Fatalf("metric %q had value %d, want %d", name, point.Value, want)
					}
					return
				}
			}
		}
	}
	t.Fatalf("metric %q with attrs %#v not found", name, attrs)
}

func requireGatewayHistogramCount(t *testing.T, metrics metricdata.ResourceMetrics, name string, attrs map[string]string, want uint64) {
	t.Helper()
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q had unexpected data type %T", name, metric.Data)
			}
			for _, point := range histogram.DataPoints {
				if gatewayMetricAttributesMatch(point.Attributes, attrs) {
					if point.Count != want {
						t.Fatalf("metric %q had count %d, want %d", name, point.Count, want)
					}
					return
				}
			}
		}
	}
	t.Fatalf("metric %q with attrs %#v not found", name, attrs)
}

func gatewayMetricAttributesMatch(set attribute.Set, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	got := map[string]string{}
	for _, kv := range set.ToSlice() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			return false
		}
	}
	return true
}

func TestIdentityMiddlewareFallsBackToScopeToken(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.RegisterScopeToken("token-1", "sandbox-token", p); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	srv := NewServer(ServerConfig{Registry: reg})
	var gotScope *SandboxScope
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotScope, _ = ScopeFromContext(r.Context())
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.64.10:12345"
	req.Header.Set(ScopeTokenHeader, "token-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotScope == nil {
		t.Fatal("expected scope in context")
	}
	if gotScope.SandboxID != "sandbox-token" {
		t.Fatalf("expected sandbox-token, got %s", gotScope.SandboxID)
	}
}

func TestIdentityMiddlewareFallsBackToScopeTokenFromConfiguredTrustedSubnet(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.RegisterScopeToken("token-1", "sandbox-token", p); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	srv := NewServer(ServerConfig{
		Registry:                        reg,
		ScopeTokenTrustedSourcePrefixes: ScopeTokenTrustedSourcePrefixesForGatewayHost("10.24.7.1"),
	})
	var gotScope *SandboxScope
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotScope, _ = ScopeFromContext(r.Context())
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.24.7.42:12345"
	req.Header.Set(ScopeTokenHeader, "token-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotScope == nil {
		t.Fatal("expected scope in context")
	}
	if gotScope.SandboxID != "sandbox-token" {
		t.Fatalf("expected sandbox-token, got %s", gotScope.SandboxID)
	}
}

func TestIdentityMiddlewareFallsBackToScopeTokenWhenSourceTrustIsDisabled(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.RegisterScopeToken("token-1", "sandbox-token", p); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	srv := NewServer(ServerConfig{
		Registry:                     reg,
		AllowScopeTokenFromAnySource: true,
	})
	var gotScope *SandboxScope
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotScope, _ = ScopeFromContext(r.Context())
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "172.16.1.100:12345"
	req.Header.Set(ScopeTokenHeader, "token-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotScope == nil {
		t.Fatal("expected scope in context")
	}
	if gotScope.SandboxID != "sandbox-token" {
		t.Fatalf("expected sandbox-token, got %s", gotScope.SandboxID)
	}
}

func TestIdentityMiddlewareRejectsScopeTokenFromUntrustedSource(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.RegisterScopeToken("token-1", "sandbox-token", p); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	srv := NewServer(ServerConfig{Registry: reg})

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "172.16.1.100:12345"
	req.Header.Set(ScopeTokenHeader, "token-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestIdentityMiddlewareRejectsUnknownScopeToken(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	srv := NewServer(ServerConfig{Registry: reg})

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "172.16.1.100:12345"
	req.Header.Set(ScopeTokenHeader, "unknown-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestScopeTokenTrustedSourcePrefixesForGatewayHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		want []netip.Prefix
	}{
		{
			name: "default host",
			host: "",
			want: []netip.Prefix{netip.MustParsePrefix("192.168.64.0/24")},
		},
		{
			name: "configured ipv4 host",
			host: "10.24.7.1",
			want: []netip.Prefix{netip.MustParsePrefix("10.24.7.0/24")},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ScopeTokenTrustedSourcePrefixesForGatewayHost(tt.host)
			if len(got) != len(tt.want) {
				t.Fatalf("len(prefixes) = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("prefix[%d] = %s, want %s", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestScopeTokenTrustedSourcePrefixesForGatewayHostResolvesHostnames(t *testing.T) {
	t.Parallel()

	lookupCalls := 0
	got := scopeTokenSourcePolicyForGatewayHost(context.Background(), "gateway.local", func(_ context.Context, host string) ([]net.IP, error) {
		lookupCalls++
		if host != "gateway.local" {
			t.Fatalf("unexpected host lookup: %q", host)
		}
		return []net.IP{
			net.ParseIP("10.24.7.1"),
			net.ParseIP("10.24.7.44"),
			net.ParseIP("fd00::1"),
		}, nil
	})

	if lookupCalls != 1 {
		t.Fatalf("lookupCalls = %d, want 1", lookupCalls)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.24.7.0/24"),
		netip.MustParsePrefix("fd00::/64"),
	}
	if len(got.TrustedSourcePrefixes) != len(want) {
		t.Fatalf("len(prefixes) = %d, want %d", len(got.TrustedSourcePrefixes), len(want))
	}
	for i := range got.TrustedSourcePrefixes {
		if got.TrustedSourcePrefixes[i] != want[i] {
			t.Fatalf("prefix[%d] = %s, want %s", i, got.TrustedSourcePrefixes[i], want[i])
		}
	}
}

func TestScopeTokenSourcePolicyForGatewayHostAllowsAnySourceOnLookupFailure(t *testing.T) {
	t.Parallel()

	got := scopeTokenSourcePolicyForGatewayHost(context.Background(), "gateway.local", func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("lookup failed")
	})

	if !got.AllowScopeTokenFromAnySource {
		t.Fatal("expected source trust to be disabled on lookup failure")
	}
	if len(got.TrustedSourcePrefixes) != 0 {
		t.Fatalf("expected no trusted source prefixes when trust is disabled, got %v", got.TrustedSourcePrefixes)
	}
}

func TestPathMiddlewareRejectsTraversal(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := srv.pathMiddleware(inner)

	req := httptest.NewRequest("GET", "/git/../secrets/key", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestStubHandlerReturns501(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.Register("10.1.1.2", "sandbox-1", p); err != nil {
		t.Fatalf("register: %v", err)
	}

	srv := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0",
		Registry:   reg,
	})

	req := httptest.NewRequest("GET", "/secrets/key", nil)
	req.RemoteAddr = "10.1.1.2:12345"
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if got := string(body); got != "secrets service not yet implemented" {
		t.Fatalf("unexpected body: %q", got)
	}
}
