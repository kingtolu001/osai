package ledgercore

import (
	"errors"
	"fmt"
)

// LedgerAccount is the in-memory financial account used by the Phase 1 ledger truth layer.
type LedgerAccount struct {
	Code      string
	Owner     string
	Currency  string
	Available int64
	Held      int64
}

// JournalEntry is a single balanced ledger movement for one ledger account and one external ref.
type JournalEntry struct {
	AccountCode string
	AccountName string
	Owner       string
	Currency    string
	Debit       int64
	Credit      int64
	ExternalRef string
}

// Journal is an immutable posting bundle used to preserve journal truth.
type Journal struct {
	ID       string
	TradeID  string
	Currency string
	Entries  []JournalEntry
}

func NewJournal(id, tradeID, currency string) *Journal {
	return &Journal{ID: id, TradeID: tradeID, Currency: currency, Entries: make([]JournalEntry, 0)}
}

func (j *Journal) AddEntry(accountCode, accountName, owner, currency string, debit, credit int64, externalRef string) {
	j.Entries = append(j.Entries, JournalEntry{
		AccountCode: accountCode,
		AccountName: accountName,
		Owner:       owner,
		Currency:    currency,
		Debit:       debit,
		Credit:      credit,
		ExternalRef: externalRef,
	})
}

func (j *Journal) Validate() error {
	if len(j.Entries) == 0 {
		return errors.New("journal cannot be empty")
	}

	seen := map[string]struct{}{}
	for _, e := range j.Entries {
		if e.ExternalRef != "" {
			if _, ok := seen[e.ExternalRef]; ok {
				return fmt.Errorf("duplicate external reference: %s", e.ExternalRef)
			}
			seen[e.ExternalRef] = struct{}{}
		}
		if e.Currency != j.Currency {
			return fmt.Errorf("currency mismatch on entry %s: %s != %s", e.AccountCode, e.Currency, j.Currency)
		}
	}

	var delta int64
	for _, e := range j.Entries {
		delta += e.Debit - e.Credit
	}
	if delta != 0 {
		return fmt.Errorf("journal %s does not balance: %d", j.ID, delta)
	}
	return nil
}

// PlaceHold reserves available funds and ensures the reserve does not exceed available funds.
func PlaceHold(account *LedgerAccount, amount int64) error {
	if amount < 0 {
		return errors.New("hold amount cannot be negative")
	}
	if account.Available < amount {
		return fmt.Errorf("hold %d exceeds available %d for %s", amount, account.Available, account.Code)
	}
	account.Available -= amount
	account.Held += amount
	return nil
}

// ReleaseHold restores a held amount back to available and enforces no over-release.
func ReleaseHold(account *LedgerAccount, amount int64) error {
	if amount < 0 {
		return errors.New("release amount cannot be negative")
	}
	if account.Held < amount {
		return fmt.Errorf("release %d exceeds held %d for %s", amount, account.Held, account.Code)
	}
	account.Held -= amount
	account.Available += amount
	return nil
}
