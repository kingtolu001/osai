package tradecore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/osai/osai/pkg/postgres"
)

func NewPostgresStore(db *sql.DB) (*Store, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS trades(id text PRIMARY KEY,institution_id text NOT NULL,quote_id text NOT NULL UNIQUE,body jsonb NOT NULL);`)
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
		if old.InstitutionID != trade.InstitutionID || old.BaseAmountMinor != trade.BaseAmountMinor || old.BaseCurrency != trade.BaseCurrency || old.QuoteCurrency != trade.QuoteCurrency || old.BeneficiaryID != trade.BeneficiaryID || old.SettlementProviderID != trade.SettlementProviderID {
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
	if _, err = tx.Exec(`INSERT INTO trades(id,institution_id,quote_id,body) VALUES($1,$2,$3,$4)`, trade.ID, trade.InstitutionID, trade.QuoteID, string(raw)); err != nil {
		return nil, err
	}
	return trade, tx.Commit()
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
