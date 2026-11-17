package main

import "net/http"

// The agent can inspect and test the existing receiver, but configuration
// remains exclusively on the private operator listener.
func (b *Bridge) handleWebhookStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.RuntimeWebhook == nil {
			writeError(w, http.StatusNotFound, "Runtime webhook is disabled")
			return
		}
		writeJSON(w, http.StatusOK, b.webhookStatus())
	}
}
