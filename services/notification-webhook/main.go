package main

import (
	"log"
	"net/http"
	"os"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/notification-webhook/webhookcore"
	"go.temporal.io/sdk/client"
)

func main() {
	if err := observability.Init("notification-webhook"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	port := os.Getenv("OSAI_WEBHOOK_PORT")
	if port == "" { port = ":8083" }
	temporalClient, err := client.Dial(client.Options{HostPort: "localhost:7233"})
	if err != nil {
		log.Fatal(err)
	}
	defer temporalClient.Close()
	pair := firstpair.New()
	handler := webhookcore.NewHandler(map[string]provider.SettlementRail{"sim_lp_1": pair.Settlement}, webhookcore.TemporalSignaler{Client: temporalClient})
	server := &http.Server{Addr: port, Handler: handler}
	log.Printf("provider webhook ingress listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
