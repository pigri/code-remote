// ngrok-forward establishes an ngrok tunnel (via the ngrok Go SDK) at a
// reserved domain and forwards incoming traffic to a local upstream — by
// default the Synapse WAF on :8080, which in turn proxies to the API. This
// replaces running the external `ngrok` agent.
//
//	NGROK_AUTHTOKEN=...  NGROK_DOMAIN=your-domain.ngrok.dev  ngrok-forward
//
// Env:
//
//	NGROK_AUTHTOKEN  (required) ngrok agent token
//	NGROK_DOMAIN     (required) reserved domain, e.g. your-domain.ngrok.dev
//	NGROK_UPSTREAM   (default http://localhost:8080) local upstream to forward to
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"golang.ngrok.com/ngrok/v2"
)

func main() {
	token := os.Getenv("NGROK_AUTHTOKEN")
	if token == "" {
		log.Fatal("NGROK_AUTHTOKEN is required")
	}
	domain := os.Getenv("NGROK_DOMAIN")
	if domain == "" {
		log.Fatal("NGROK_DOMAIN is required (your reserved ngrok domain)")
	}
	upstream := os.Getenv("NGROK_UPSTREAM")
	if upstream == "" {
		upstream = "http://localhost:8080" // Synapse WAF
	}

	agent, err := ngrok.NewAgent(ngrok.WithAuthtoken(token))
	if err != nil {
		log.Fatalf("ngrok agent: %v", err)
	}

	// Stop the tunnel cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fwd, err := agent.Forward(ctx,
		ngrok.WithUpstream(upstream),
		ngrok.WithURL(domain),
	)
	if err != nil {
		log.Fatalf("ngrok forward: %v", err)
	}

	log.Printf("ngrok forwarding %s -> %s", fwd.URL(), upstream)
	<-fwd.Done()
}
