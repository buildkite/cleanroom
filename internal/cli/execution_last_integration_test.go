package cli

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"unsafe"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func TestExecutionInspectGlobalLastUsesSelectedSandboxID(t *testing.T) {
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Message:     "done",
			}, nil
		},
	}

	host, svc := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	firstSandboxID := mustCreateSandbox(t, client)
	secondSandboxID := mustCreateSandbox(t, client)

	firstResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: firstSandboxID,
		Command:   []string{"echo", "first"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution first returned error: %v", err)
	}
	firstExecutionID := firstResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), firstSandboxID, firstExecutionID); err != nil {
		t.Fatalf("WaitExecution first returned error: %v", err)
	}

	secondResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: secondSandboxID,
		Command:   []string{"echo", "second"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution second returned error: %v", err)
	}
	secondExecutionID := secondResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), secondSandboxID, secondExecutionID); err != nil {
		t.Fatalf("WaitExecution second returned error: %v", err)
	}

	forceExecutionID(t, svc, secondSandboxID, secondExecutionID, firstExecutionID)

	outcome := runExecutionInspectWithCapture(ExecutionInspectCommand{
		clientFlags: clientFlags{Host: host},
		Last:        true,
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecutionInspectCommand.Run returned error: %v", outcome.err)
	}

	assertContainsAll(
		t,
		outcome.stdout,
		"execution: "+firstExecutionID,
		"sandbox: "+secondSandboxID,
		"status: succeeded",
	)
}

func forceExecutionID(t *testing.T, svc *controlservice.Service, sandboxID, fromExecutionID, toExecutionID string) {
	t.Helper()

	mu := rwMutexField(t, svc, "mu")
	mu.Lock()
	defer mu.Unlock()

	executionsField := reflect.ValueOf(svc).Elem().FieldByName("executions")
	executionsValue := reflect.NewAt(executionsField.Type(), unsafe.Pointer(executionsField.UnsafeAddr())).Elem()
	oldKey := reflect.ValueOf(sandboxID + "/" + fromExecutionID)
	newKey := reflect.ValueOf(sandboxID + "/" + toExecutionID)
	entry := executionsValue.MapIndex(oldKey)
	if !entry.IsValid() || entry.IsNil() {
		t.Fatalf("execution entry %q missing", sandboxID+"/"+fromExecutionID)
	}

	executionValue := reflect.NewAt(entry.Elem().Type(), unsafe.Pointer(entry.Pointer())).Elem()
	idField := executionValue.FieldByName("ID")
	reflect.NewAt(idField.Type(), unsafe.Pointer(idField.UnsafeAddr())).Elem().SetString(toExecutionID)
	executionsValue.SetMapIndex(newKey, entry)
	executionsValue.SetMapIndex(oldKey, reflect.Value{})
}

func rwMutexField(t *testing.T, svc *controlservice.Service, name string) *sync.RWMutex {
	t.Helper()

	field := reflect.ValueOf(svc).Elem().FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("service field %q missing", name)
	}
	return (*sync.RWMutex)(unsafe.Pointer(field.UnsafeAddr()))
}
