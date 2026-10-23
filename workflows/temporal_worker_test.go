package workflows

import "testing"

func TestProofTaskQueuePreservesProductionDefaultAndRejectsUnsafeOverrides(t *testing.T) {
	t.Setenv("OSAI_TEMPORAL_TASK_QUEUE", "")
	t.Setenv("OSAI_LOCAL_PROOF_MODE", "")
	if queue, err := ConfiguredTaskQueue(); err != nil || queue != TaskQueue {
		t.Fatalf("default changed: %s %v", queue, err)
	}
	t.Setenv("OSAI_TEMPORAL_TASK_QUEUE", "osai-proof-test")
	if _, err := ConfiguredTaskQueue(); err == nil {
		t.Fatal("production queue override accepted")
	}
	t.Setenv("OSAI_LOCAL_PROOF_MODE", "true")
	if queue, err := ConfiguredTaskQueue(); err != nil || queue != "osai-proof-test" {
		t.Fatalf("proof queue rejected: %s %v", queue, err)
	}
	for _, queue := range []string{TaskQueue, "osai-proof-", "osai-proof-test other", "osai-proof-test/other"} {
		t.Setenv("OSAI_TEMPORAL_TASK_QUEUE", queue)
		if _, err := ConfiguredTaskQueue(); err == nil {
			t.Fatal("unsafe proof queue accepted")
		}
	}
}
