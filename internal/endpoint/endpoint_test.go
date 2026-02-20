package endpoint

import "testing"

func TestResolveTSNetEndpoint(t *testing.T) {
	t.Parallel()

	ep, err := Resolve("tsnet://cleanroomd:8443")
	if err != nil {
		t.Fatalf("resolve tsnet endpoint: %v", err)
	}

	if ep.Scheme != "tsnet" {
		t.Fatalf("expected tsnet scheme, got %q", ep.Scheme)
	}
	if ep.Address != ":8443" {
		t.Fatalf("expected listen address :8443, got %q", ep.Address)
	}
	if ep.BaseURL != "http://cleanroomd:8443" {
		t.Fatalf("expected base url http://cleanroomd:8443, got %q", ep.BaseURL)
	}
	if ep.TSNetHostname != "cleanroomd" {
		t.Fatalf("expected hostname cleanroomd, got %q", ep.TSNetHostname)
	}
	if ep.TSNetPort != 8443 {
		t.Fatalf("expected port 8443, got %d", ep.TSNetPort)
	}
}

func TestResolveTSNetEndpointDefaults(t *testing.T) {
	t.Parallel()

	ep, err := Resolve("tsnet://")
	if err != nil {
		t.Fatalf("resolve tsnet endpoint with defaults: %v", err)
	}

	if ep.Address != ":7777" {
		t.Fatalf("expected default listen address :7777, got %q", ep.Address)
	}
	if ep.BaseURL != "http://cleanroom:7777" {
		t.Fatalf("expected default base url http://cleanroom:7777, got %q", ep.BaseURL)
	}
	if ep.TSNetHostname != "cleanroom" {
		t.Fatalf("expected default hostname cleanroom, got %q", ep.TSNetHostname)
	}
	if ep.TSNetPort != 7777 {
		t.Fatalf("expected default port 7777, got %d", ep.TSNetPort)
	}
}

func TestResolveTSNetEndpointRejectsInvalidPort(t *testing.T) {
	t.Parallel()

	if _, err := Resolve("tsnet://cleanroomd:99999"); err == nil {
		t.Fatal("expected invalid tsnet port to fail")
	}
}

func TestResolveTSNetEndpointRejectsPath(t *testing.T) {
	t.Parallel()

	if _, err := Resolve("tsnet://cleanroomd:8443/path"); err == nil {
		t.Fatal("expected tsnet endpoint with path to fail")
	}
}
