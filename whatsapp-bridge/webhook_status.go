package main

import "net/http"

func (b *Bridge) registerWebhookEndpoints(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	// Neither route uses an SDK client. Keep them outside the client handoff
	// gate, including the bounded wait for a synthetic receiver response.
	mux.HandleFunc("/api/webhook", auth(b.webhookToolGuard(requireMethod(http.MethodGet, b.handleWebhookStatus()))))
	mux.HandleFunc("/api/webhook/test", auth(b.ReadOnly.guard(b.webhookToolGuard(requireMethod(http.MethodPost, b.handleRuntimeWebhookTest)))))
}

func (b *Bridge) webhookToolGuard(h http.HandlerFunc) http.HandlerFunc {
	if b.RuntimeDefaults == nil {
		return b.Tools.guard(h)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Operator idle does not remove the right to inspect webhook state.
		snapshot, err := b.settingsSnapshot(r.Context())
		if err != nil {
			writeErrorCode(w, 503, "settings_unavailable", "Runtime webhook policy unavailable")
			return
		}
		snapshot.policy.guard(h)(w, r)
	}
}

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
