package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/rancher/remotedialer"
)

const relayHost = "share.peekm.dev"

var relayURL = "wss://" + relayHost

// startTunnel connects to the relay and serves requests through the tunnel.
// Blocks until ctx is cancelled. Uses exponential backoff on reconnect.
func startTunnel(ctx context.Context, token string, localPort int) {
	headers := http.Header{
		"X-Token": []string{token},
		"X-Port":  []string{fmt.Sprintf("%d", localPort)},
	}

	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	backoff := time.Second

	for {
		err := remotedialer.ClientConnect(ctx, relayURL+"/tunnel", headers, nil,
			func(_, addr string) bool {
				return addr == localAddr || addr == fmt.Sprintf("localhost:%d", localPort)
			},
			nil,
		)
		if ctx.Err() != nil {
			return
		}
		log.Printf("Tunnel disconnected: %v, reconnecting in %v...", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
