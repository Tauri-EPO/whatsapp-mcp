package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintQRCode_FirstAndRefreshedCodes(t *testing.T) {
	var first, second bytes.Buffer
	printQRCode(&first, "2@first-code-payload,abc,def", 1)
	printQRCode(&second, "2@second-code-payload,ghi,jkl", 2)

	if !strings.Contains(first.String(), "Scan this QR code") {
		t.Fatalf("first code should carry the initial prompt; got:\n%s", first.String())
	}
	if strings.Contains(first.String(), "refreshed") {
		t.Fatal("first code must not be labelled as a refresh")
	}
	if !strings.Contains(second.String(), "QR code refreshed (#2)") {
		t.Fatalf("second code should be labelled as refresh #2; got:\n%s", second.String())
	}
	// Different payloads must render different QR blocks: the whole point of
	// redrawing is that the phone rejects a scan of the rotated-out code.
	if first.String() == second.String() {
		t.Fatal("QR renderings for different codes should differ")
	}
	for _, buf := range []*bytes.Buffer{&first, &second} {
		if !strings.Contains(buf.String(), "Waiting for QR code scan") {
			t.Fatalf("missing wait hint in:\n%s", buf.String())
		}
	}
}
