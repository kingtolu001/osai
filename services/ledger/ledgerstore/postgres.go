package ledgerstore

import (
	"database/sql"

	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/ledger/ledgercore"
)

type Postgres struct{ DB *sql.DB }

func (p Postgres) SaveJournal(key string, journal ledgercore.Journal) error {
	transaction, err := p.DB.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err = transaction.Exec(`INSERT INTO ledger_journals (dedup_key, journal_id, trade_id, currency) VALUES ($1,$2,$3,$4) ON CONFLICT (dedup_key) DO NOTHING`, key, journal.ID, journal.TradeID, journal.Currency); err != nil {
		return err
	}
	for _, entry := range journal.Entries {
		if _, err = transaction.Exec(`INSERT INTO ledger_entries (journal_id, account_code, account_name, owner, currency, debit, credit, external_ref) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, journal.ID, entry.AccountCode, entry.AccountName, entry.Owner, entry.Currency, entry.Debit, entry.Credit, entry.ExternalRef); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (p Postgres) LoadJournals() (map[string]ledgercore.Journal, error) {
	rows, err := p.DB.Query(`SELECT dedup_key, journal_id, trade_id, currency FROM ledger_journals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]ledgercore.Journal)
	for rows.Next() {
		var key, id, trade, currency string
		if err := rows.Scan(&key, &id, &trade, &currency); err != nil {
			return nil, err
		}
		result[key] = ledgercore.Journal{ID: id, TradeID: trade, Currency: currency}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	entries, err := p.DB.Query(`SELECT j.dedup_key, e.journal_id, e.account_code, e.account_name, e.owner, e.currency, e.debit, e.credit, e.external_ref FROM ledger_entries e JOIN ledger_journals j ON j.journal_id = e.journal_id ORDER BY e.id`)
	if err != nil {
		return nil, err
	}
	defer entries.Close()
	for entries.Next() {
		var key, journalID, accountCode, accountName, owner, currency, externalRef string
		var debit, credit int64
		if err := entries.Scan(&key, &journalID, &accountCode, &accountName, &owner, &currency, &debit, &credit, &externalRef); err != nil {
			return nil, err
		}
		journal := result[key]
		journal.Entries = append(journal.Entries, ledgercore.JournalEntry{AccountCode: accountCode, AccountName: accountName, Owner: owner, Currency: currency, Debit: debit, Credit: credit, ExternalRef: externalRef})
		result[key] = journal
	}
	return result, entries.Err()
}

var _ ledgerapi.Storage = Postgres{}
