package ids

import "github.com/google/uuid"

func NewUUID() string { return uuid.NewString() }

func NewTradeID() string      { return "trd_" + uuid.NewString() }
func NewQuoteID() string      { return "quo_" + uuid.NewString() }
func NewSettlementID() string { return "si_" + uuid.NewString() }
func NewJournalID() string    { return "jt_" + uuid.NewString() }
