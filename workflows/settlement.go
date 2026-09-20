package workflows

import (
	"errors"
	"sync"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type Stage string

const (
	StageCreated          Stage = "CREATED"
	StageAwaitingEvidence Stage = "AWAITING_EVIDENCE"
	StageManualReview     Stage = "MANUAL_REVIEW"
	StageTerminal         Stage = "TERMINAL"
)

type Snapshot struct {
	InstructionID string
	ClientRef     string
	Stage         Stage
	LastError     string
}

type SnapshotStore interface {
	Save(Snapshot) error
	Load(instructionID string) (Snapshot, bool)
}

type MemorySnapshots struct {
	mu    sync.Mutex
	items map[string]Snapshot
}

func NewMemorySnapshots() *MemorySnapshots { return &MemorySnapshots{items: make(map[string]Snapshot)} }
func (s *MemorySnapshots) Save(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[snapshot.InstructionID] = snapshot
	return nil
}
func (s *MemorySnapshots) Load(id string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.items[id]
	return snapshot, ok
}

type SettlementWorkflow struct {
	Store     *settlementcore.Store
	Rail      provider.SettlementRail
	Snapshots SnapshotStore
}

func (w *SettlementWorkflow) Start(beneficiary, currency string, amountMinor int64, purpose string) (settlementcore.Instruction, error) {
	if w.Store == nil || w.Rail == nil || w.Snapshots == nil {
		return settlementcore.Instruction{}, errors.New("workflow dependencies are required")
	}
	instruction, err := w.Store.Create(beneficiary, currency, amountMinor, purpose)
	if err != nil {
		return settlementcore.Instruction{}, err
	}
	if err = w.Snapshots.Save(Snapshot{InstructionID: instruction.ID, ClientRef: instruction.ClientRef, Stage: StageCreated}); err != nil {
		return settlementcore.Instruction{}, err
	}
	return instruction, nil
}

// Submit is an activity boundary. Its snapshot is persisted before the provider call by the settlement store.
func (w *SettlementWorkflow) Submit(id string) (settlementcore.Instruction, error) {
	instruction, err := w.Store.Submit(id, w.Rail)
	if err != nil {
		return instruction, w.saveFailure(id, err)
	}
	return instruction, w.saveStage(instruction)
}

// Resume is safe after a worker or service restart because the instruction and client_ref are loaded from durable state.
func (w *SettlementWorkflow) Resume(id string) (settlementcore.Instruction, error) {
	instruction, ok := w.Store.Get(id)
	if !ok {
		return settlementcore.Instruction{}, errors.New("settlement instruction not found")
	}
	if !instruction.State.Terminal() && instruction.State != settlementcore.ManualReview {
		var err error
		instruction, err = w.Store.Poll(id, w.Rail)
		if err != nil {
			return instruction, w.saveFailure(id, err)
		}
	}
	return instruction, w.saveStage(instruction)
}

func (w *SettlementWorkflow) HandleWebhook(id string, event provider.WebhookEvent) (settlementcore.Instruction, error) {
	instruction, err := w.Store.ResolveWebhook(id, event)
	if err != nil {
		return instruction, w.saveFailure(id, err)
	}
	return instruction, w.saveStage(instruction)
}

func (w *SettlementWorkflow) Escalate(id, reason string) (settlementcore.Instruction, error) {
	instruction, err := w.Store.EscalateUnknown(id, reason)
	if err != nil {
		return instruction, w.saveFailure(id, err)
	}
	return instruction, w.saveStage(instruction)
}

func (w *SettlementWorkflow) saveStage(instruction settlementcore.Instruction) error {
	stage := StageAwaitingEvidence
	if instruction.State.Terminal() {
		stage = StageTerminal
	}
	if instruction.State == settlementcore.ManualReview {
		stage = StageManualReview
	}
	return w.Snapshots.Save(Snapshot{InstructionID: instruction.ID, ClientRef: instruction.ClientRef, Stage: stage})
}

func (w *SettlementWorkflow) saveFailure(id string, err error) error {
	if w.Snapshots == nil {
		return err
	}
	snapshot, _ := w.Snapshots.Load(id)
	snapshot.InstructionID = id
	snapshot.LastError = err.Error()
	_ = w.Snapshots.Save(snapshot)
	return err
}
