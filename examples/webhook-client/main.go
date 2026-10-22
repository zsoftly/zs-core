// Command webhook-client is a reference receiver for ZS Core webhooks.
// Keep seen event IDs in durable storage in production to make at-least-once
// delivery safe across process restarts.
package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/zsoftly/zs-core/pkg/notifications"
)

func main() {
	secret := []byte(os.Getenv("ZS_WEBHOOK_SECRET"))
	if len(secret) == 0 {
		log.Fatal("ZS_WEBHOOK_SECRET is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/webhooks/zs", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil || notifications.Verify(secret, body, r.Header.Get(notifications.SignatureHeader), time.Now()) != nil {
			http.Error(w, "invalid webhook", http.StatusUnauthorized)
			return
		}
		// Decode the versioned events.Envelope, then atomically record its evt_
		// ID before applying business effects. Repeated IDs are successful no-ops.
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Addr: "127.0.0.1:8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	log.Fatal(server.ListenAndServe())
}
