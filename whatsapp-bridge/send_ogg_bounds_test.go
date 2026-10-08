package main

import "testing"

func TestOggAnalysisRefusesTruncatedPageBody(t *testing.T) {
	data := oggPage(0, 0, opusHead(0, 48000))
	data = data[: len(data)-1 : len(data)-1]
	if _, _, err := analyzeOggOpus(data); err == nil {
		t.Fatal("truncated Ogg page body was accepted")
	}
}
