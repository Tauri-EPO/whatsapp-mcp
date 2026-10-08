package main

import "testing"

func TestOggAnalysisBoundsTruncatedPageBody(t *testing.T) {
	data := oggPage(0, 0, opusHead(0, 48000))
	data = data[: len(data)-1 : len(data)-1]
	seconds, waveform, err := analyzeOggOpus(data)
	if err != nil || seconds != 1 || len(waveform) != 64 {
		t.Fatalf("partial-page analysis=%d seconds/%d waveform bytes, err=%v", seconds, len(waveform), err)
	}
}
