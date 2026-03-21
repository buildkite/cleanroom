package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveExecutionEnv(t *testing.T) {
	t.Setenv("INHERITED_ENV", "from-host")

	got, err := resolveExecutionEnv([]string{"INHERITED_ENV", "EXPLICIT=value", "EMPTY="})
	if err != nil {
		t.Fatalf("resolveExecutionEnv returned error: %v", err)
	}

	want := []string{"INHERITED_ENV=from-host", "EXPLICIT=value", "EMPTY="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected resolved env: got %v want %v", got, want)
	}
}

func TestResolveExecutionEnvRejectsUnsetInheritedVar(t *testing.T) {
	_, err := resolveExecutionEnv([]string{"UNSET_ENV_FOR_TEST"})
	if err == nil {
		t.Fatal("expected resolveExecutionEnv to fail for unset inherited variable")
	}
	if !strings.Contains(err.Error(), "UNSET_ENV_FOR_TEST") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveExecutionEnvRejectsMissingExplicitKey(t *testing.T) {
	_, err := resolveExecutionEnv([]string{"=value"})
	if err == nil {
		t.Fatal("expected resolveExecutionEnv to fail for missing key")
	}
	if !strings.Contains(err.Error(), "missing variable name") {
		t.Fatalf("unexpected error: %v", err)
	}
}
