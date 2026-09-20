package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/workflows"
	"go.temporal.io/sdk/client"
)

func main() {
	mode := flag.String("mode", "start", "start or signal")
	clientRef := flag.String("client-ref", "si_runtime_drill", "stable client reference")
	state := flag.String("state", "CONFIRMED", "signal state: CONFIRMED or FAILED")
	maxPolls := flag.Int("max-polls", 0, "maximum one-second recovery polls; zero waits indefinitely for evidence")
	flag.Parse()
	connection, err := client.Dial(client.Options{HostPort: "localhost:7233"})
	if err != nil {
		log.Fatal(err)
	}
	defer connection.Close()
	workflowID := "settlement-" + *clientRef
	switch *mode {
	case "start":
		run, err := connection.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{ID: workflowID, TaskQueue: workflows.TaskQueue}, workflows.TemporalSettlementWorkflow, workflows.TemporalSettlementInput{InstructionID: *clientRef, ClientRef: *clientRef, Beneficiary: "acct_runtime", AmountMinor: 100, Currency: "USD", MaxPolls: *maxPolls})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("started", run.GetID(), run.GetRunID())
	case "signal":
		status := provider.TransferConfirmed
		if *state == "FAILED" {
			status = provider.TransferFailed
		}
		event := provider.WebhookEvent{ProviderEventID: "evt_" + *clientRef, ClientRef: *clientRef, Status: status, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_runtime"}
		if err := connection.SignalWorkflow(context.Background(), workflowID, "", workflows.CallbackSignal, event); err != nil {
			log.Fatal(err)
		}
		fmt.Println("signalled", workflowID, *state)
	case "query":
		var result workflows.TemporalSettlementResult
		if err := connection.GetWorkflow(context.Background(), workflowID, "").Get(context.Background(), &result); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("completed %s polls=%d ledger_post=%s\n", result.State, result.Polls, result.LedgerPost)
	default:
		log.Fatalf("unknown mode %s", *mode)
	}
}
