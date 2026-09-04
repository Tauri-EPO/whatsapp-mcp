package main

import (
	"testing"
	"time"
)

func TestRememberAndTakeOriginalTimestamp(t *testing.T) {
	reg := newOriginalTimestamps()

	orig := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	reg.remember("MSG_A", orig)

	got, ok := reg.take("MSG_A")
	if !ok {
		t.Fatal("expected cached timestamp for MSG_A")
	}
	if !got.Equal(orig) {
		t.Fatalf("got %s, want %s", got, orig)
	}

	// A message ID is consumed on take: a second take must miss.
	if _, ok := reg.take("MSG_A"); ok {
		t.Fatal("expected MSG_A to be consumed after take")
	}
}

func TestRememberKeepsEarliest(t *testing.T) {
	reg := newOriginalTimestamps()

	earliest := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	later := earliest.Add(12 * time.Hour) // e.g. the retry-resend time

	// Order of observation must not matter: the earliest (original) always wins.
	reg.remember("MSG_B", later)
	reg.remember("MSG_B", earliest)
	reg.remember("MSG_B", later)

	got, ok := reg.take("MSG_B")
	if !ok {
		t.Fatal("expected cached timestamp for MSG_B")
	}
	if !got.Equal(earliest) {
		t.Fatalf("got %s, want earliest %s", got, earliest)
	}
}

func TestRememberIgnoresEmptyInput(t *testing.T) {
	reg := newOriginalTimestamps()

	reg.remember("", time.Now())       // empty ID
	reg.remember("MSG_C", time.Time{}) // zero time

	if _, ok := reg.take(""); ok {
		t.Fatal("empty ID should never be cached")
	}
	if _, ok := reg.take("MSG_C"); ok {
		t.Fatal("zero timestamp should never be cached")
	}
}

func TestTakeMissingReturnsFalse(t *testing.T) {
	reg := newOriginalTimestamps()
	if _, ok := reg.take("does-not-exist"); ok {
		t.Fatal("expected miss for unknown ID")
	}
}
