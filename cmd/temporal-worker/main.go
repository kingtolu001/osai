package main

import (
	"log"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/services/settlement/ledgerclient"
	"github.com/osai/osai/workflows"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
)

func main() {
	temporalClient, err := client.Dial(client.Options{HostPort: "localhost:7233"})
	if err != nil {
		log.Fatal(err)
	}
	defer temporalClient.Close()
	ledgerConnection, err := grpc.Dial("localhost:50051", grpc.WithInsecure())
	if err != nil {
		log.Fatal(err)
	}
	defer ledgerConnection.Close()
	pair := firstpair.New()
	activities := &workflows.ProviderSettlementActivities{Rail: pair.Settlement, Ledger: ledgerclient.New(ledgerConnection)}
	worker, err := workflows.StartSettlementWorker(nil, temporalClient, activities)
	if err != nil {
		log.Fatal(err)
	}
	defer worker.Stop()
	log.Println("Temporal settlement worker listening on task queue", workflows.TaskQueue)
	select {}
}
