package cli

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultContentCacheStorageUsesXDGCacheHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg-cache")
	got, err := defaultContentCacheStorage()
	if err != nil {
		t.Fatalf("default storage: %v", err)
	}
	if want := filepath.Join("/tmp/xdg-cache", "cleanroom", "content-cache"); got != want {
		t.Fatalf("storage = %q, want %q", got, want)
	}
}

func TestContentCacheDefaultHosts(t *testing.T) {
	if got := defaultStrings(nil, defaultContentCacheGitHosts); !reflect.DeepEqual(got, []string{"github.com"}) {
		t.Fatalf("default git hosts = %#v", got)
	}
	if got := defaultStrings([]string{"gitlab.com"}, defaultContentCacheGitHosts); !reflect.DeepEqual(got, []string{"gitlab.com"}) {
		t.Fatalf("explicit git hosts = %#v", got)
	}
}
