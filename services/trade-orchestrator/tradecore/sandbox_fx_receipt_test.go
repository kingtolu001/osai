package tradecore

import (
	"testing"

	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
)

func TestSandboxFXPostingReceiptRequiresCommittedRevision(t *testing.T) {
	const command = `{"EventID":"sbx:posting","Event":"fx_complete","ExpectedRevision":1}`
	for _, replay := range []bool{false, true} {
		for _, revision := range []int64{0, 1, 2, 3} {
			receipt := &ledgerv1.SandboxAccountingResponse{EventId: "sbx:posting", State: "FX_HELD", Revision: revision, AlreadyApplied: replay}
			err := validateSandboxFXPostingReceipt("sbx:posting", command, receipt)
			if (err == nil) != (revision == 2) {
				t.Fatalf("revision=%d replay=%t: %v", revision, replay, err)
			}
		}
	}
	for _, raw := range []string{`{`, `{}`, `{"EventID":"other","Event":"fx_complete","ExpectedRevision":1}`, `{"EventID":"sbx:posting","Event":"reserve","ExpectedRevision":1}`} {
		if validateSandboxFXPostingReceipt("sbx:posting", raw, &ledgerv1.SandboxAccountingResponse{EventId: "sbx:posting", State: "FX_HELD", Revision: 2}) == nil {
			t.Fatal("acknowledged a receipt for an invalid or different command")
		}
	}
}
