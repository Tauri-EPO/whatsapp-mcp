package main

import (
	"testing"
	"time"
	_ "time/tzdata" // zone database for the container images without tzdata

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// The cached file name is computed twice: on arrival from the whatsmeow
// timestamp (extractMediaInfo) and later from the timestamp read back out of
// SQLite (mediaFileName, used by downloadMedia and the purge). Both must agree
// whatever TZ the process runs in, otherwise a re-download misses the cache and
// a purge never finds the file. Python's scan_chat_cache matches by message id
// and is unaffected.
func TestMediaFileName_AgreesWithArrivalNameAcrossTimezones(t *testing.T) {
	ms := newTestMessageStore(t)
	chat := "5511999999999@s.whatsapp.net"
	if err := ms.StoreChat(chat, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	url := "https://mmg.whatsapp.net/v/x.enc"
	length := uint64(10)
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: &url, MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: &length}}

	zones := []string{"UTC", "America/Sao_Paulo", "Asia/Kolkata", "Pacific/Auckland"}
	instants := []int64{
		1757000000, // ordinary
		time.Date(2026, 11, 1, 2, 30, 0, 0, time.UTC).Unix(), // around a DST switch in Sao Paulo's old rules / NZ
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),   // midnight UTC = previous day in the Americas
	}
	for _, zone := range zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		prev := time.Local
		time.Local = loc
		t.Cleanup(func() { time.Local = prev })
		for i, sec := range instants {
			id := zone + "-" + string(rune('A'+i))
			arrival := time.Unix(sec, 0) // what whatsmeow hands to handleMessage: a Local time
			_, arrivalName, _, _, _, _, _ := extractMediaInfo(msg, arrival, id)
			if err := ms.StoreMessage(id, chat, "x", "", arrival, false, "image", "", url, []byte("k"), []byte("s"), []byte("e"), length, ""); err != nil {
				t.Fatal(err)
			}
			row, err := ms.MediaRow(id, chat)
			if err != nil {
				t.Fatal(err)
			}
			later := mediaFileName(row.MediaType, row.Timestamp, row.ID, "")
			if later != arrivalName {
				t.Errorf("%s: arrival %q vs re-download %q (row ts %v)", id, arrivalName, later, row.Timestamp)
			}
			// Guard against a vacuous pass: outside UTC the wall-clock digits must
			// really differ from the UTC rendering, so the comparison above means
			// something.
			utcName := mediaFileName("image", time.Unix(sec, 0).UTC(), id, "")
			if zone != "UTC" && utcName == arrivalName {
				t.Errorf("%s: expected a non-UTC wall clock in the name, got %q", id, arrivalName)
			}
		}
		time.Local = prev
	}
}
