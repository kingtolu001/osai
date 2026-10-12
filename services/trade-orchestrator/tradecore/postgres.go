package tradecore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/osai/osai/pkg/postgres"
	"time"
)

func NewPostgresStore(db *sql.DB) (*Store, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS trades(id text PRIMARY KEY,institution_id text NOT NULL,quote_id text NOT NULL UNIQUE,body jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),status text NOT NULL DEFAULT '',base_currency text NOT NULL DEFAULT '',customer_reference text NOT NULL DEFAULT '');
	ALTER TABLE trades ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
	ALTER TABLE trades ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT '';
	ALTER TABLE trades ADD COLUMN IF NOT EXISTS base_currency text NOT NULL DEFAULT '';
	ALTER TABLE trades ADD COLUMN IF NOT EXISTS customer_reference text NOT NULL DEFAULT '';
	UPDATE trades SET created_at=COALESCE(NULLIF(body->>'CreatedAt','')::timestamptz,created_at),status=COALESCE(NULLIF(body->>'Status',''),status),base_currency=COALESCE(NULLIF(body->>'BaseCurrency',''),base_currency),customer_reference=COALESCE(body->>'CustomerReference',customer_reference);
	CREATE INDEX IF NOT EXISTS trades_institution_created_idx ON trades(institution_id,created_at DESC,id DESC);
	CREATE INDEX IF NOT EXISTS trades_institution_status_idx ON trades(institution_id,status);
	CREATE TABLE IF NOT EXISTS trade_transitions(id bigserial PRIMARY KEY,trade_id text NOT NULL REFERENCES trades(id),from_state text NOT NULL,to_state text NOT NULL,occurred_at timestamptz NOT NULL DEFAULT now());
	CREATE TABLE IF NOT EXISTS trade_settlement_event_inbox(event_id text PRIMARY KEY,institution_id text NOT NULL,settlement_id text NOT NULL,trade_id text NOT NULL REFERENCES trades(id),status text NOT NULL CHECK(status IN ('SETTLED','FAILED')),correlation_id text NOT NULL DEFAULT '',received_at timestamptz NOT NULL DEFAULT now());
	ALTER TABLE trade_settlement_event_inbox ADD COLUMN IF NOT EXISTS correlation_id text NOT NULL DEFAULT '';`)
	if err != nil {
		return nil, err
	}
	s := NewStore()
	s.db = db
	return s, nil
}
func (s *Store) createDB(trade *Trade) (*Trade, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = postgres.Lock(tx, "trade:"+trade.QuoteID); err != nil {
		return nil, err
	}
	var raw []byte
	err = tx.QueryRow(`SELECT body FROM trades WHERE quote_id=$1`, trade.QuoteID).Scan(&raw)
	if err == nil {
		var old Trade
		if err = json.Unmarshal(raw, &old); err != nil {
			return nil, err
		}
		if old.InstitutionID != trade.InstitutionID || old.BaseAmountMinor != trade.BaseAmountMinor || old.BaseCurrency != trade.BaseCurrency || old.QuoteCurrency != trade.QuoteCurrency || old.BeneficiaryID != trade.BeneficiaryID || old.SettlementProviderID != trade.SettlementProviderID || old.CustomerReference != trade.CustomerReference {
			return nil, errors.New("trade replay conflict")
		}
		return &old, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	raw, err = json.Marshal(trade)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO trades(id,institution_id,quote_id,body,created_at,status,base_currency,customer_reference) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, trade.ID, trade.InstitutionID, trade.QuoteID, string(raw), trade.CreatedAt, trade.Status, trade.BaseCurrency, trade.CustomerReference); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO trade_transitions(trade_id,from_state,to_state) VALUES($1,'', $2)`, trade.ID, trade.Status); err != nil {
		return nil, err
	}
	return trade, tx.Commit()
}

func (s *Store) listTradesDB(institutionID string, filter TradeListFilter) ([]Trade, int, error) {
	var from, to any
	if !filter.From.IsZero() {
		from = filter.From
	}
	if !filter.To.IsZero() {
		to = filter.To
	}
	args := []any{institutionID, filter.Status, filter.Currency, filter.CustomerReference, from, to}
	const where = `institution_id=$1 AND ($2='' OR status=$2) AND ($3='' OR upper(base_currency)=upper($3)) AND ($4='' OR customer_reference=$4) AND ($5::timestamptz IS NULL OR created_at >= $5) AND ($6::timestamptz IS NULL OR created_at < $6)`
	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM trades WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	offset := (filter.Page - 1) * filter.PageSize
	rows, err := s.db.Query(`SELECT body FROM trades WHERE `+where+` ORDER BY created_at DESC,id DESC LIMIT $7 OFFSET $8`, append(args, filter.PageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	trades := make([]Trade, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, 0, err
		}
		var trade Trade
		if err := json.Unmarshal(raw, &trade); err != nil {
			return nil, 0, err
		}
		trades = append(trades, trade)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return trades, total, nil
}

func (s *Store) getDB(field, id string) (*Trade, bool) {
	query := `SELECT body FROM trades WHERE id=$1`
	if field == "quote" {
		query = `SELECT body FROM trades WHERE quote_id=$1`
	}
	var raw []byte
	if s.db.QueryRow(query, id).Scan(&raw) != nil {
		return nil, false
	}
	var trade Trade
	if json.Unmarshal(raw, &trade) != nil {
		return nil, false
	}
	return &trade, true
}
func (s *Store) setSettlementDB(tradeID, settlementID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRow(`SELECT body FROM trades WHERE id=$1 FOR UPDATE`, tradeID).Scan(&raw); err != nil {
		return err
	}
	var trade Trade
	if err = json.Unmarshal(raw, &trade); err != nil {
		return err
	}
	if trade.SettlementID != "" && trade.SettlementID != settlementID {
		return errors.New("settlement conflict")
	}
	trade.SettlementID = settlementID
	raw, err = json.Marshal(trade)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE trades SET body=$2 WHERE id=$1`, tradeID, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) setStatusDB(tradeID string, state TradeState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRow(`SELECT body FROM trades WHERE id=$1 FOR UPDATE`, tradeID).Scan(&raw); err != nil {
		return err
	}
	var trade Trade
	if err = json.Unmarshal(raw, &trade); err != nil {
		return err
	}
	previous := trade.Status
	if previous == string(state) {
		return tx.Commit()
	}
	trade.State, trade.Status, trade.UpdatedAt = state, string(state), time.Now().UTC()
	raw, err = json.Marshal(trade)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE trades SET body=$2,status=$3 WHERE id=$1`, tradeID, raw, trade.Status); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO trade_transitions(trade_id,from_state,to_state) VALUES($1,$2,$3)`, tradeID, previous, string(state)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) applySettlementFinalityDB(event FinalityEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRow(`SELECT body FROM trades WHERE id=$1 FOR UPDATE`, event.TradeID).Scan(&raw); err != nil {
		return err
	}
	var trade Trade
	if err = json.Unmarshal(raw, &trade); err != nil {
		return err
	}
	if trade.InstitutionID != event.InstitutionID || trade.SettlementID != event.SettlementID || trade.CorrelationID != event.CorrelationID {
		return errors.New("settlement finality identity mismatch")
	}
	var prior FinalityEvent
	err = tx.QueryRow(`SELECT event_id,institution_id,settlement_id,trade_id,status,correlation_id FROM trade_settlement_event_inbox WHERE event_id=$1`, event.EventID).
		Scan(&prior.EventID, &prior.InstitutionID, &prior.SettlementID, &prior.TradeID, &prior.Status, &prior.CorrelationID)
	if err == nil {
		if prior != event {
			return errors.New("settlement finality event collision")
		}
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	if trade.State != StateSettling && trade.State != event.Status {
		return errors.New("settlement finality conflicts with trade state")
	}
	if _, err = tx.Exec(`INSERT INTO trade_settlement_event_inbox(event_id,institution_id,settlement_id,trade_id,status,correlation_id) VALUES($1,$2,$3,$4,$5,$6)`, event.EventID, event.InstitutionID, event.SettlementID, event.TradeID, event.Status, event.CorrelationID); err != nil {
		return err
	}
	if trade.State != event.Status {
		previous := trade.Status
		trade.State, trade.Status, trade.UpdatedAt = event.Status, string(event.Status), time.Now().UTC()
		raw, err = json.Marshal(&trade)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE trades SET body=$2,status=$3 WHERE id=$1`, event.TradeID, raw, trade.Status); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO trade_transitions(trade_id,from_state,to_state) VALUES($1,$2,$3)`, event.TradeID, previous, event.Status); err != nil {
			return err
		}
	}
	return tx.Commit()
}
