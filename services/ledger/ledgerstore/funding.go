package ledgerstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/ledger/ledgercore"
)

// All funding mutations take the same institution/currency transaction lock.
// The journal and reservation row commit together; a process restart cannot
// leave an active hold without its economic reclassification.
func fundingLock(ctx context.Context, tx *sql.Tx, institution, currency string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "funding:"+institution+":"+currency)
	return err
}

func insertFundingJournal(ctx context.Context, tx *sql.Tx, key, kind string, journal *ledgercore.Journal) error {
	if err := journal.Validate(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_journals(dedup_key,journal_id,trade_id,institution_id,currency,kind) VALUES($1,$2,$3,$4,$5,$6)`, key, journal.ID, journal.TradeID, journal.InstitutionID, journal.Currency, kind); err != nil {
		return err
	}
	for _, entry := range journal.Entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries(journal_id,account_code,account_name,owner,currency,debit,credit,external_ref) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, journal.ID, entry.AccountCode, entry.AccountName, entry.Owner, entry.Currency, entry.Debit, entry.Credit, entry.ExternalRef); err != nil {
			return err
		}
	}
	return nil
}

func (p Postgres) PostLocalPrefund(ctx context.Context, cmd ledgerapi.PrefundCommand) (string, bool, error) {
	if p.CustodyBankID == "" || cmd.InstitutionID == "" || cmd.EvidenceReference == "" || cmd.Currency == "" || cmd.AmountMinor <= 0 {
		return "", false, errors.New("invalid prefund evidence")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if err = fundingLock(ctx, tx, cmd.InstitutionID, cmd.Currency); err != nil {
		return "", false, err
	}
	var institution, currency, journalID string
	var amount int64
	err = tx.QueryRowContext(ctx, `SELECT institution_id,currency,amount_minor,journal_id FROM ledger_prefund_evidence WHERE evidence_reference=$1`, cmd.EvidenceReference).Scan(&institution, &currency, &amount, &journalID)
	if err == nil {
		if institution != cmd.InstitutionID || currency != cmd.Currency || amount != cmd.AmountMinor {
			return "", false, ledgerapi.ErrIdempotencyConflict
		}

		return journalID, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	journal := ledgercore.NewJournal("jt_prefund_"+cmd.EvidenceReference, "prefund:"+cmd.EvidenceReference, cmd.Currency)
	journal.InstitutionID = cmd.InstitutionID
	journal.AddEntry("1300", "CLIENT_FUNDS_BANK", p.CustodyBankID, cmd.Currency, cmd.AmountMinor, 0, cmd.EvidenceReference+":bank")
	journal.AddEntry("2000", "CLIENT_PREFUND", cmd.InstitutionID, cmd.Currency, 0, cmd.AmountMinor, cmd.EvidenceReference+":institution")
	if err = insertFundingJournal(ctx, tx, "prefund:"+cmd.EvidenceReference, "prefund", journal); err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_prefund_evidence(evidence_reference,institution_id,currency,amount_minor,journal_id) VALUES($1,$2,$3,$4,$5)`, cmd.EvidenceReference, cmd.InstitutionID, cmd.Currency, cmd.AmountMinor, journal.ID); err != nil {
		return "", false, err
	}
	return journal.ID, false, tx.Commit()
}

func (p Postgres) ReserveFunding(ctx context.Context, cmd ledgerapi.FundingCommand) (ledgerapi.FundingReservation, bool, error) {
	if cmd.InstitutionID == "" || cmd.TradeID == "" || cmd.Currency == "" || cmd.AmountMinor <= 0 {
		return ledgerapi.FundingReservation{}, false, errors.New("invalid funding command")
	}
	var snapshotJSON string
	if cmd.PricingSnapshot != nil {
		if err := pricing.Validate(*cmd.PricingSnapshot); err != nil {
			return ledgerapi.FundingReservation{}, false, fmt.Errorf("invalid funding pricing snapshot: %w", err)
		}
		hash, err := pricing.Hash(*cmd.PricingSnapshot)
		if err != nil {
			return ledgerapi.FundingReservation{}, false, err
		}
		if hash != cmd.PricingSnapshot.Hash || hash != cmd.PricingHash ||
			cmd.PricingSnapshot.TotalDebitMinor != cmd.AmountMinor ||
			cmd.PricingSnapshot.SourcePrincipalMinor != cmd.SourcePrincipalMinor ||
			cmd.PricingSnapshot.FeeMinor != cmd.FeeMinor ||
			cmd.PricingSnapshot.SourceCurrency != cmd.Currency {
			return ledgerapi.FundingReservation{}, false, errors.New("funding command does not match immutable pricing snapshot")
		}
		serialized, err := json.Marshal(cmd.PricingSnapshot)
		if err != nil {
			return ledgerapi.FundingReservation{}, false, fmt.Errorf("serialize funding pricing snapshot: %w", err)
		}
		snapshotJSON = string(serialized)
	} else if cmd.PricingHash != "" {
		return ledgerapi.FundingReservation{}, false, errors.New("complete pricing snapshot required")
	}
	if cmd.SourcePrincipalMinor == 0 && cmd.FeeMinor == 0 && cmd.PricingHash == "" {
		cmd.SourcePrincipalMinor = cmd.AmountMinor
	}
	if cmd.FeeMinor > 0 && p.sandboxFees && (cmd.Currency != "NGN" || cmd.PricingSnapshot == nil) {
		return ledgerapi.FundingReservation{}, false, errors.New("sandbox fee policy requires an NGN pricing snapshot")
	}
	if cmd.SourcePrincipalMinor <= 0 || cmd.FeeMinor < 0 ||
		cmd.SourcePrincipalMinor > cmd.AmountMinor || cmd.FeeMinor != cmd.AmountMinor-cmd.SourcePrincipalMinor ||
		(cmd.PricingHash != "" && len(cmd.PricingHash) != 64) {
		return ledgerapi.FundingReservation{}, false, errors.New("invalid funding pricing terms")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	defer tx.Rollback()
	if err = fundingLock(ctx, tx, cmd.InstitutionID, cmd.Currency); err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	var old ledgerapi.FundingReservation
	var oldSnapshotJSON, oldPolicy string
	err = tx.QueryRowContext(ctx, `SELECT trade_id,institution_id,currency,amount_minor,source_principal_minor,fee_minor,pricing_hash,pricing_snapshot_json,state,journal_id,settlement_id,settlement_ref,accounting_policy FROM ledger_funding_reservations WHERE trade_id=$1`, cmd.TradeID).Scan(&old.TradeID, &old.InstitutionID, &old.Currency, &old.AmountMinor, &old.SourcePrincipalMinor, &old.FeeMinor, &old.PricingHash, &oldSnapshotJSON, &old.State, &old.JournalID, &old.SettlementID, &old.SettlementRef, &oldPolicy)
	if err == nil {
		if old.InstitutionID != cmd.InstitutionID || old.Currency != cmd.Currency || old.AmountMinor != cmd.AmountMinor ||
			old.SourcePrincipalMinor != cmd.SourcePrincipalMinor || old.FeeMinor != cmd.FeeMinor || old.PricingHash != cmd.PricingHash {
			return ledgerapi.FundingReservation{}, false, ledgerapi.ErrIdempotencyConflict
		}
		if oldSnapshotJSON != snapshotJSON || (old.FeeMinor > 0 && oldPolicy != p.feePolicy()) {
			return ledgerapi.FundingReservation{}, false, ledgerapi.ErrIdempotencyConflict
		}
		if oldSnapshotJSON != "" {
			var persisted pricing.Snapshot
			if err := json.Unmarshal([]byte(oldSnapshotJSON), &persisted); err != nil {
				return ledgerapi.FundingReservation{}, false, fmt.Errorf("decode persisted funding pricing snapshot: %w", err)
			}
			old.PricingSnapshot = &persisted
		}
		return old, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ledgerapi.FundingReservation{}, false, err
	}
	var available int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(credit-debit),0) FROM ledger_entries WHERE account_code='2000' AND owner=$1 AND currency=$2`, cmd.InstitutionID, cmd.Currency).Scan(&available); err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	if available < cmd.AmountMinor {
		return ledgerapi.FundingReservation{}, false, ledgerapi.ErrInsufficientFunding
	}
	journal := ledgercore.NewJournal("jt_hold_"+cmd.TradeID, cmd.TradeID, cmd.Currency)
	journal.InstitutionID = cmd.InstitutionID
	journal.AddEntry("2000", "CLIENT_PREFUND", cmd.InstitutionID, cmd.Currency, cmd.AmountMinor, 0, cmd.TradeID+":hold:available")
	journal.AddEntry("2100", "CLIENT_PREFUND_HELD", cmd.InstitutionID, cmd.Currency, 0, cmd.AmountMinor, cmd.TradeID+":hold:reserved")
	if err = insertFundingJournal(ctx, tx, "hold:"+cmd.TradeID, "hold", journal); err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_funding_reservations(trade_id,institution_id,currency,amount_minor,source_principal_minor,fee_minor,pricing_hash,pricing_snapshot_json,state,journal_id,accounting_policy) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ACTIVE',$9,$10)`, cmd.TradeID, cmd.InstitutionID, cmd.Currency, cmd.AmountMinor, cmd.SourcePrincipalMinor, cmd.FeeMinor, cmd.PricingHash, snapshotJSON, journal.ID, p.feePolicy()); err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	reservation := ledgerapi.FundingReservation{TradeID: cmd.TradeID, InstitutionID: cmd.InstitutionID, Currency: cmd.Currency, AmountMinor: cmd.AmountMinor, SourcePrincipalMinor: cmd.SourcePrincipalMinor, FeeMinor: cmd.FeeMinor, PricingHash: cmd.PricingHash, PricingSnapshot: cmd.PricingSnapshot, State: "ACTIVE", JournalID: journal.ID}
	return reservation, false, tx.Commit()
}

func (p Postgres) GetFundingReservation(ctx context.Context, cmd ledgerapi.FundingCommand) (ledgerapi.FundingReservation, error) {
	var r ledgerapi.FundingReservation
	var snapshotJSON, policy string
	err := p.DB.QueryRowContext(ctx, `SELECT trade_id,institution_id,currency,amount_minor,source_principal_minor,fee_minor,pricing_hash,pricing_snapshot_json,state,journal_id,settlement_id,settlement_ref,accounting_policy FROM ledger_funding_reservations WHERE trade_id=$1`, cmd.TradeID).Scan(&r.TradeID, &r.InstitutionID, &r.Currency, &r.AmountMinor, &r.SourcePrincipalMinor, &r.FeeMinor, &r.PricingHash, &snapshotJSON, &r.State, &r.JournalID, &r.SettlementID, &r.SettlementRef, &policy)
	if err != nil {
		return ledgerapi.FundingReservation{}, err
	}
	if snapshotJSON != "" {
		var snapshot pricing.Snapshot
		if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
			return ledgerapi.FundingReservation{}, fmt.Errorf("decode persisted funding pricing snapshot: %w", err)
		}
		hash, err := pricing.Hash(snapshot)
		if err != nil {
			return ledgerapi.FundingReservation{}, err
		}
		if err := pricing.Validate(snapshot); err != nil || hash != r.PricingHash ||
			snapshot.TotalDebitMinor != r.AmountMinor ||
			snapshot.SourcePrincipalMinor != r.SourcePrincipalMinor ||
			snapshot.FeeMinor != r.FeeMinor || snapshot.SourceCurrency != r.Currency {
			return ledgerapi.FundingReservation{}, errors.New("persisted funding pricing snapshot is inconsistent")
		}
		r.PricingSnapshot = &snapshot
	}
	if r.InstitutionID != cmd.InstitutionID || r.Currency != cmd.Currency || (r.AmountMinor != cmd.AmountMinor && !(p.sandboxFees && policy == SandboxAccountingVersion && r.FeeMinor > 0 && r.SourcePrincipalMinor == cmd.AmountMinor)) {
		return ledgerapi.FundingReservation{}, ledgerapi.ErrFundingMismatch
	}
	if cmd.PricingHash != "" && (r.PricingHash != cmd.PricingHash ||
		r.SourcePrincipalMinor != cmd.SourcePrincipalMinor || r.FeeMinor != cmd.FeeMinor) {
		return ledgerapi.FundingReservation{}, ledgerapi.ErrFundingMismatch
	}
	if cmd.PricingSnapshot != nil {
		if err := pricing.Validate(*cmd.PricingSnapshot); err != nil {
			return ledgerapi.FundingReservation{}, err
		}
		expected, err := json.Marshal(cmd.PricingSnapshot)
		if err != nil {
			return ledgerapi.FundingReservation{}, err
		}
		if string(expected) != snapshotJSON {
			return ledgerapi.FundingReservation{}, ledgerapi.ErrFundingMismatch
		}
	}
	return r, nil
}

func (p Postgres) FinalizeFunding(ctx context.Context, cmd ledgerapi.Command, confirmed bool) (string, bool, error) {
	if p.CustodyBankID == "" || cmd.InstitutionID == "" || cmd.TradeID == "" || cmd.SettlementID == "" || cmd.ExternalReference == "" || cmd.AmountMinor <= 0 || cmd.Currency == "" {
		return "", false, errors.New("invalid funded settlement command")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if err = fundingLock(ctx, tx, cmd.InstitutionID, cmd.Currency); err != nil {
		return "", false, err
	}
	var r ledgerapi.FundingReservation
	var policy, storedSnapshot string
	err = tx.QueryRowContext(ctx, `SELECT trade_id,institution_id,currency,amount_minor,source_principal_minor,fee_minor,pricing_hash,state,journal_id,settlement_id,settlement_ref,accounting_policy,pricing_snapshot_json FROM ledger_funding_reservations WHERE trade_id=$1 FOR UPDATE`, cmd.TradeID).Scan(&r.TradeID, &r.InstitutionID, &r.Currency, &r.AmountMinor, &r.SourcePrincipalMinor, &r.FeeMinor, &r.PricingHash, &r.State, &r.JournalID, &r.SettlementID, &r.SettlementRef, &policy, &storedSnapshot)
	if err != nil {
		return "", false, fmt.Errorf("funding reservation required: %w", err)
	}
	if r.InstitutionID != cmd.InstitutionID || r.Currency != cmd.Currency || (r.FeeMinor == 0 && r.AmountMinor != cmd.AmountMinor) || (r.FeeMinor > 0 && (!p.sandboxFees || policy != SandboxAccountingVersion || r.SourcePrincipalMinor != cmd.AmountMinor)) {
		return "", false, ledgerapi.ErrFundingMismatch
	}
	if r.SettlementID != "" && (r.SettlementID != cmd.SettlementID || r.SettlementRef != cmd.ExternalReference) {
		return "", false, ledgerapi.ErrFundingMismatch
	}

	var commandHash string
	if r.FeeMinor > 0 {
		var snapshot pricing.Snapshot
		if cmd.Currency != "NGN" || cmd.IdempotencyKey == "" || cmd.CorrelationID == "" || cmd.ProviderReference == "" || cmd.Beneficiary == "" || cmd.Purpose == "" || json.Unmarshal([]byte(storedSnapshot), &snapshot) != nil {
			return "", false, ledgerapi.ErrFundingMismatch
		}
		hash, hashErr := pricing.Hash(snapshot)
		if hashErr != nil || pricing.Validate(snapshot) != nil || hash != r.PricingHash || snapshot.Hash != hash || snapshot.SourceCurrency != r.Currency || snapshot.DestinationCurrency != r.Currency || snapshot.TotalDebitMinor != r.AmountMinor || snapshot.SourcePrincipalMinor != r.SourcePrincipalMinor || snapshot.FeeMinor != r.FeeMinor {
			return "", false, ledgerapi.ErrFundingMismatch
		}
		encoded, _ := json.Marshal(cmd)
		digest := sha256.Sum256(encoded)
		commandHash = hex.EncodeToString(digest[:])
	}
	key := "settlement:" + cmd.TradeID
	var journalID string
	err = tx.QueryRowContext(ctx, `SELECT journal_id FROM ledger_journals WHERE dedup_key=$1`, key).Scan(&journalID)
	if err == nil {
		if confirmed && r.State != "CONSUMED" || !confirmed && r.State != "RELEASED" {
			return "", false, ledgerapi.ErrFundingMismatch
		}

		if r.FeeMinor > 0 {
			var existingHash string
			if err := tx.QueryRowContext(ctx, `SELECT command_hash FROM ledger_fee_finality WHERE trade_id=$1`, cmd.TradeID).Scan(&existingHash); err != nil {
				return "", false, err
			}
			if existingHash != commandHash {
				return "", false, ledgerapi.ErrIdempotencyConflict
			}
		}
		return journalID, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if r.State != "ACTIVE" {
		return "", false, errors.New("funding reservation is not active")
	}
	journal := ledgercore.NewJournal("jt_"+cmd.ExternalReference, cmd.TradeID, cmd.Currency)
	journal.InstitutionID = cmd.InstitutionID
	if confirmed {
		// Same-currency payout: consume the held client claim into a payable,
		// then settle that payable against safeguarded custody (JT-105).
		journal.AddEntry("2100", "CLIENT_PREFUND_HELD", cmd.InstitutionID, cmd.Currency, cmd.AmountMinor, 0, cmd.ExternalReference+":held")
		journal.AddEntry("2200", "CLIENT_SETTLEMENT_PAYABLE", cmd.InstitutionID, cmd.Currency, 0, cmd.AmountMinor, cmd.ExternalReference+":payable")
		journal.AddEntry("2200", "CLIENT_SETTLEMENT_PAYABLE", cmd.InstitutionID, cmd.Currency, cmd.AmountMinor, 0, cmd.ExternalReference+":discharge")
		journal.AddEntry("1300", "CLIENT_FUNDS_BANK", p.CustodyBankID, cmd.Currency, 0, cmd.AmountMinor, cmd.ExternalReference+":custody")
		if r.FeeMinor > 0 {
			journal.AddEntry("2100", "CLIENT_PREFUND_HELD", cmd.InstitutionID, cmd.Currency, r.FeeMinor, 0, cmd.ExternalReference+":fee:held")
			journal.AddEntry("sbx:NGN:fee_revenue", "SANDBOX_SERVICE_FEE", "sbx:osai", cmd.Currency, 0, r.FeeMinor, cmd.ExternalReference+":fee:earned")
		}
	} else {
		journal.AddEntry("2100", "CLIENT_PREFUND_HELD", cmd.InstitutionID, cmd.Currency, r.AmountMinor, 0, cmd.ExternalReference+":release")
		journal.AddEntry("2000", "CLIENT_PREFUND", cmd.InstitutionID, cmd.Currency, 0, r.AmountMinor, cmd.ExternalReference+":available")
	}
	kind := "settlement.failed"
	if confirmed {
		kind = "settlement.confirmed"
	}
	if err = insertFundingJournal(ctx, tx, key, kind, journal); err != nil {
		return "", false, err
	}
	state := "RELEASED"
	if confirmed {
		state = "CONSUMED"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ledger_funding_reservations SET state=$2,settlement_id=$3,settlement_ref=$4 WHERE trade_id=$1`, cmd.TradeID, state, cmd.SettlementID, cmd.ExternalReference); err != nil {
		return "", false, err
	}

	if r.FeeMinor > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_fee_finality(trade_id,policy_version,command_hash,correlation_id,provider_ref,service_actor,reason) VALUES($1,$2,$3,$4,$5,'local-sandbox-ledger',$6)`, cmd.TradeID, policy, commandHash, cmd.CorrelationID, cmd.ProviderReference, cmd.Purpose); err != nil {
			return "", false, err
		}
	}
	return journal.ID, false, tx.Commit()
}

func (p Postgres) feePolicy() string {
	if p.sandboxFees {
		return SandboxAccountingVersion
	}
	return ""
}
