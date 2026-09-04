package main

import (
	"net/http"
)

// Liveness (/api/health) and readiness (/api/ready). REST starts before
// pairing, so health is 200 as soon as the listener is up and ready is 200
// only while paired and connected.

// handleHealth serves GET /api/health.
func (b *Bridge) handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, b.healthStatus())
	}
}

// handleReady serves GET /api/ready.
func (b *Bridge) handleReady() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := b.healthStatus()
		code := http.StatusOK
		if status["status"] != "ok" {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, status)
	}
}
