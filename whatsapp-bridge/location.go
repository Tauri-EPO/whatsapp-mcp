package main

import (
	"encoding/json"
	"math"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
)

// Locations have no downloadable file. Old rendered text is deliberately not
// parsed: its separators cannot recover the original fields unambiguously.
type messageLocation struct {
	Live      bool     `json:"live"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Name      string   `json:"name,omitempty"`
	Address   string   `json:"address,omitempty"`
	URL       string   `json:"url,omitempty"`
	Comment   string   `json:"comment,omitempty"`
	Accuracy  *uint32  `json:"accuracy_meters,omitempty"`
	Speed     *float32 `json:"speed_mps,omitempty"`
	Bearing   *uint32  `json:"bearing_degrees,omitempty"`
	Sequence  *int64   `json:"sequence,omitempty"`
	Offset    *uint32  `json:"time_offset_seconds,omitempty"`
}

func locationOf(m *waE2E.Message) *messageLocation {
	var p *messageLocation
	if l := m.GetLocationMessage(); l != nil {
		p = &messageLocation{Live: l.GetIsLive(), Latitude: l.DegreesLatitude, Longitude: l.DegreesLongitude,
			Name: l.GetName(), Address: l.GetAddress(), URL: l.GetURL(), Comment: l.GetComment(),
			Accuracy: l.AccuracyInMeters, Speed: l.SpeedInMps, Bearing: l.DegreesClockwiseFromMagneticNorth}
	} else if l := m.GetLiveLocationMessage(); l != nil {
		p = &messageLocation{Live: true, Latitude: l.DegreesLatitude, Longitude: l.DegreesLongitude,
			Comment: l.GetCaption(), Accuracy: l.AccuracyInMeters, Speed: l.SpeedInMps,
			Bearing: l.DegreesClockwiseFromMagneticNorth, Sequence: l.SequenceNumber, Offset: l.TimeOffset}
	}
	if p == nil {
		return nil
	}
	if formatCoordinates(p.Latitude, p.Longitude) == "" {
		p.Latitude, p.Longitude = nil, nil
	}
	if p.Speed != nil && (math.IsNaN(float64(*p.Speed)) || math.IsInf(float64(*p.Speed), 0) || *p.Speed < 0) {
		p.Speed = nil
	}
	if p.Bearing != nil && *p.Bearing > 360 {
		p.Bearing = nil
	}
	return p
}

func (p *messageLocation) column() any {
	if p == nil {
		return nil
	}
	data, _ := json.Marshal(p) // ingress removes the non-finite numeric values
	return string(data)
}

func (p *messageLocation) update() bool {
	return p != nil && p.Live && p.Sequence != nil && *p.Sequence > 0
}

// The exact (id, chat) key is the only relationship we infer. A distinct ID
// remains a distinct archived row; no heuristic drops another share's sample.
// A same-key update changes position only, preserving the first row's content,
// author, timestamp, quote and mentions. Out-of-order samples cannot go backwards.
func updateLiveLocationWith(ex sqlExecer, id, chat string, p *messageLocation) (bool, error) {
	if id == "" || !p.update() {
		return false, nil
	}
	position := *p
	position.Name, position.Address, position.URL, position.Comment = "", "", "", ""
	result, err := ex.Exec(`UPDATE messages SET location = CASE
		WHEN COALESCE(json_extract(location, '$.sequence'), 0) < ? THEN json_patch(location, ?) ELSE location END
		WHERE id = ? AND chat_jid = ? AND media_type = 'location'
		AND CASE WHEN json_valid(location) THEN json_extract(location, '$.live') = 1 ELSE 0 END`,
		*p.Sequence, position.column(), id, chat)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *MessageStore) UpdateLiveLocation(id, chat string, p *messageLocation) (bool, error) {
	return updateLiveLocationWith(s.db, id, chat, p)
}

func (b *messageBatch) UpdateLiveLocation(id, chat string, p *messageLocation) (bool, error) {
	matched := false
	err := b.write(func() error {
		var err error
		matched, err = updateLiveLocationWith(b.tx, id, chat, p)
		return err
	})
	return matched, err
}

// A history position update is not fresh conversation activity. Only the
// location case changes the usual newest-first timestamp rule; other history
// chunks keep their existing marker and retry behavior.
func (b *Bridge) historyLocationActivityTime(rows []*waHistorySync.HistorySyncMsg, chat string, fallback time.Time) time.Time {
	if len(rows) == 0 || !extractMessage(rows[0].GetMessage().GetMessage(), fallback, rows[0].GetMessage().GetKey().GetID()).location.update() {
		return fallback
	}
	initial := make(map[string]time.Time)
	for _, row := range rows {
		info := row.GetMessage()
		p := extractMessage(info.GetMessage(), fallback, info.GetKey().GetID()).location
		if p != nil && p.Live && !p.update() && info.GetKey().GetID() != "" && info.GetMessageTimestamp() != 0 {
			stamp := time.Unix(int64(info.GetMessageTimestamp()), 0) //nolint:gosec // WhatsApp epoch seconds
			old, found := initial[info.GetKey().GetID()]
			if !found || stamp.Before(old) {
				initial[info.GetKey().GetID()] = stamp
			}
		}
	}
	var latest time.Time
	for _, row := range rows {
		info := row.GetMessage()
		if info.GetMessageTimestamp() == 0 {
			continue
		}
		stamp := time.Unix(int64(info.GetMessageTimestamp()), 0) //nolint:gosec // same WhatsApp seconds as the canonical importer
		if extractMessage(info.GetMessage(), fallback, info.GetKey().GetID()).location.update() && info.GetKey().GetID() != "" {
			var original time.Time
			if first, found := initial[info.GetKey().GetID()]; found {
				stamp = first
			} else if err := b.Store.db.QueryRow(`SELECT timestamp FROM messages WHERE id = ? AND chat_jid = ? AND media_type = 'location'
				AND CASE WHEN json_valid(location) THEN json_extract(location, '$.live') = 1 ELSE 0 END`, info.GetKey().GetID(), chat).Scan(&original); err == nil {
				stamp = original
			}
		}
		if stamp.After(latest) {
			latest = stamp
		}
	}
	if latest.IsZero() {
		return fallback
	}
	return latest
}
