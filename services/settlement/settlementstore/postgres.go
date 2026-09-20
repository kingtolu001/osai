package settlementstore

import (
	"database/sql"
	"time"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type Postgres struct{ DB *sql.DB }

func (p Postgres) SaveInstruction(instruction settlementcore.Instruction) error {
	_, err := p.DB.Exec(`INSERT INTO settlement_instructions (id, client_ref, provider_ref, beneficiary, amount_minor, currency, purpose, state, supersedes, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (id) DO UPDATE SET provider_ref=EXCLUDED.provider_ref, state=EXCLUDED.state, supersedes=EXCLUDED.supersedes, updated_at=EXCLUDED.updated_at`, instruction.ID, instruction.ClientRef, instruction.ProviderRef, instruction.Beneficiary, instruction.AmountMinor, instruction.Currency, instruction.Purpose, instruction.State, instruction.Supersedes, instruction.CreatedAt, instruction.UpdatedAt)
	return err
}

func (p Postgres) SaveTransition(id string, transition settlementcore.Transition) error {
	_, err := p.DB.Exec(`INSERT INTO settlement_transitions (settlement_id, from_state, to_state, event, reason, source, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, transition.From, transition.To, transition.Event, transition.Reason, transition.Source, transition.OccurredAt)
	return err
}

func (p Postgres) SaveEvidence(id string, evidence provider.Evidence) error {
	_, err := p.DB.Exec(`INSERT INTO settlement_evidence (evidence_id, settlement_id, provider, method, request_ref, raw_payload_hash, parsed_at, correlation_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (evidence_id) DO NOTHING`, evidence.ID, id, evidence.Provider, evidence.Method, evidence.RequestRef, evidence.RawPayloadHash, evidence.ParsedAt, evidence.CorrelationID)
	return err
}

func (p Postgres) LoadInstructions() ([]settlementcore.Instruction, error) {
	rows, err := p.DB.Query(`SELECT id, client_ref, provider_ref, beneficiary, amount_minor, currency, purpose, state, supersedes, created_at, updated_at FROM settlement_instructions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]settlementcore.Instruction, 0)
	for rows.Next() {
		var instruction settlementcore.Instruction
		var state string
		var supersedes sql.NullString
		if err := rows.Scan(&instruction.ID, &instruction.ClientRef, &instruction.ProviderRef, &instruction.Beneficiary, &instruction.AmountMinor, &instruction.Currency, &instruction.Purpose, &state, &supersedes, &instruction.CreatedAt, &instruction.UpdatedAt); err != nil {
			return nil, err
		}
		instruction.State = settlementcore.State(state)
		instruction.Supersedes = supersedes.String
		result = append(result, instruction)
	}
	return result, rows.Err()
}

var _ settlementcore.Persistence = Postgres{}
var _ = time.Time{}
