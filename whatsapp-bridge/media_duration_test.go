package main

import (
	"fmt"
	"testing"

	"go.mau.fi/whatsmeow"
)

// Granules and pre-skip count 48 kHz samples, whatever the encoder input rate.
// The extra 312 samples must not round a three-second clip up to four seconds.
func TestVoiceNoteDurationUsesGranuleClock(t *testing.T) {
	installRecordingLogger(t)
	for _, rate := range []uint32{0, 8000, 16000, 24000, 48000, 192000} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			data := append(oggPage(0, 0, opusHead(312, rate)), oggPage(1, 3*48000+312, []byte{0xfc})...)
			duration, wave, err := analyzeOggOpus(data)
			if err != nil || duration != 3 || len(wave) != 64 {
				t.Fatalf("duration=%d waveform=%d error=%v; want 3 seconds and 64 samples", duration, len(wave), err)
			}
			msg, err := buildMediaMessage(whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "/out/voice.ogg", data, testUpload(), "")
			if err != nil || msg.AudioMessage.GetSeconds() != 3 || len(msg.AudioMessage.GetWaveform()) != 64 {
				t.Fatalf("built voice note=%v error=%v", msg, err)
			}
		})
	}
}

func TestVoiceNoteGranuleEdgeCases(t *testing.T) {
	installRecordingLogger(t)
	for _, tc := range []struct {
		name    string
		granule uint64
		want    uint32
	}{
		{"no completed packet", ^uint64(0), 1},
		{"pre-skip exceeds granule", 10, 1},
		{"enormous granule clamps before integer conversion", ^uint64(0) - 1, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(oggPage(0, 0, opusHead(312, 24000)), oggPage(1, tc.granule, []byte{0xfc})...)
			got, wave, err := analyzeOggOpus(data)
			if err != nil || got != tc.want || len(wave) != 64 {
				t.Fatalf("duration=%d waveform=%d error=%v", got, len(wave), err)
			}
		})
	}
}
