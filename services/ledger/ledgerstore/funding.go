package ledgerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	defer tx.Rollback()
	if err = fundingLock(ctx, tx, cmd.InstitutionID, cmd.Currency); err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	var old ledgerapi.FundingReservation
	err = tx.QueryRowContext(ctx, `SELECT trade_id,institution_id,currency,amount_minor,state,journal_id,settlement_id,settlement_ref FROM ledger_funding_reservations WHERE trade_id=$1`, cmd.TradeID).Scan(&old.TradeID, &old.InstitutionID, &old.Currency, &old.AmountMinor, &old.State, &old.JournalID, &old.SettlementID, &old.SettlementRef)
	if err == nil {
		if old.InstitutionID != cmd.InstitutionID || old.Currency != cmd.Currency || old.AmountMinor != cmd.AmountMinor {
			return ledgerapi.FundingReservation{}, false, ledgerapi.ErrIdempotencyConflict
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_funding_reservations(trade_id,institution_id,currency,amount_minor,state,journal_id) VALUES($1,$2,$3,$4,'ACTIVE',$5)`, cmd.TradeID, cmd.InstitutionID, cmd.Currency, cmd.AmountMinor, journal.ID); err != nil {
		return ledgerapi.FundingReservation{}, false, err
	}
	reservation := ledgerapi.FundingReservation{TradeID: cmd.TradeID, InstitutionID: cmd.InstitutionID, Currency: cmd.Currency, AmountMinor: cmd.AmountMinor, State: "ACTIVE", JournalID: journal.ID}
	return reservation, false, tx.Commit()
}

func (p Postgres) GetFundingReservation(ctx context.Context, cmd ledgerapi.FundingCommand) (ledgerapi.FundingReservation, error) {
	var r ledgerapi.FundingReservation
	err := p.DB.QueryRowContext(ctx, `SELECT trade_id,institution_id,currency,amount_minor,state,journal_id,settlement_id,settlement_ref FROM ledger_funding_reservations WHERE trade_id=$1`, cmd.TradeID).Scan(&r.TradeID, &r.InstitutionID, &r.Currency, &r.AmountMinor, &r.State, &r.JournalID, &r.SettlementID, &r.SettlementRef)
	if err != nil {
		return ledgerapi.FundingReservation{}, err
	}
	if r.InstitutionID != cmd.InstitutionID || r.Currency != cmd.Currency || r.AmountMinor != cmd.AmountMinor {
		return ledgerapi.FundingReservation{}, ledgerapi.ErrFundingMismatch
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
	err = tx.QueryRowContext(ctx, `SELECT trade_id,institution_id,currency,amount_minor,state,journal_id,settlement_id,settlement_ref FROM ledger_funding_reservations WHERE trade_id=$1 FOR UPDATE`, cmd.TradeID).Scan(&r.TradeID, &r.InstitutionID, &r.Currency, &r.AmountMinor, &r.State, &r.JournalID, &r.SettlementID, &r.SettlementRef)
	if err != nil {
		return "", false, fmt.Errorf("funding reservation required: %w", err)
	}
	if r.InstitutionID != cmd.InstitutionID || r.Currency != cmd.Currency || r.AmountMinor != cmd.AmountMinor {
		return "", false, ledgerapi.ErrFundingMismatch
	}
	if r.SettlementID != "" && (r.SettlementID != cmd.SettlementID || r.SettlementRef != cmd.ExternalReference) {
		return "", false, ledgerapi.ErrFundingMismatch
	}
	key := "settlement:" + cmd.TradeID
	var journalID string
	err = tx.QueryRowContext(ctx, `SELECT journal_id FROM ledger_journals WHERE dedup_key=$1`, key).Scan(&journalID)
	if err == nil {
		if confirmed && r.State != "CONSUMED" || !confirmed && r.State != "RELEASED" {
			return "", false, ledgerapi.ErrFundingMismatch
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
	} else {
		journal.AddEntry("2100", "CLIENT_PREFUND_HELD", cmd.InstitutionID, cmd.Currency, cmd.AmountMinor, 0, cmd.ExternalReference+":release")
		journal.AddEntry("2000", "CLIENT_PREFUND", cmd.InstitutionID, cmd.Currency, 0, cmd.AmountMinor, cmd.ExternalReference+":available")
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
	return journal.ID, false, tx.Commit()
}
