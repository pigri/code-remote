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
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"golang.ngrok.com/ngrok/v2"
)

// config is the resolved runtime configuration read from the environment.
type config struct {
	token, domain, upstream string
}

// loadConfig reads and validates the NGROK_* environment, applying the default
// upstream. Split from main so the validation is testable without the SDK.
func loadConfig() (config, error) {
	token := os.Getenv("NGROK_AUTHTOKEN")
	if token == "" {
		return config{}, errors.New("NGROK_AUTHTOKEN is required")
	}
	domain := os.Getenv("NGROK_DOMAIN")
	if domain == "" {
		return config{}, errors.New("NGROK_DOMAIN is required (your reserved ngrok domain)")
	}
	upstream := os.Getenv("NGROK_UPSTREAM")
	if upstream == "" {
		upstream = "http://localhost:8080" // Synapse WAF
	}
	return config{token: token, domain: domain, upstream: upstream}, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	agent, err := ngrok.NewAgent(ngrok.WithAuthtoken(cfg.token))
	if err != nil {
		log.Fatalf("ngrok agent: %v", err)
	}

	// Stop the tunnel cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fwd, err := agent.Forward(ctx,
		ngrok.WithUpstream(cfg.upstream),
		ngrok.WithURL(cfg.domain),
	)
	if err != nil {
		log.Fatalf("ngrok forward: %v", err)
	}

	log.Printf("ngrok forwarding %s -> %s", fwd.URL(), cfg.upstream)
	<-fwd.Done()
}
