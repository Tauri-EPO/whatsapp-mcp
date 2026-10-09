package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
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

type locationWriter interface {
	sqlExecer
	QueryRow(query string, args ...any) *sql.Row
}

type locationAuthorReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Preserve an archived LID author when a later trusted delivery resolves to
// its verified PN alias. The transaction still checks the exact stored author,
// namespace and ownership, so a row changed after this read is refused safely.
func (b *Bridge) liveLocationSender(ctx context.Context, reader locationAuthorReader, id, chat, sender string, fromMe bool) (string, error) {
	var user, server string
	var own bool
	err := reader.QueryRowContext(ctx, "SELECT sender,COALESCE(sender_server,''),is_from_me FROM messages WHERE id=? AND chat_jid=?", id, chat).Scan(&user, &server, &own)
	if errors.Is(err, sql.ErrNoRows) {
		return sender, nil
	}
	if err != nil {
		return sender, err
	}
	if server != types.HiddenUserServer || own != fromMe {
		return sender, nil
	}
	previous := types.NewJID(user, server)
	verified, err := lookupAltJID(ctx, b.Client, previous)
	if err != nil {
		return sender, err
	}
	if !verified.IsEmpty() && storedSender(verified.ToNonAD()) == sender {
		return previous.String(), nil
	}
	return sender, nil
}

// A true result consumes a known key, including an author collision. Any
// incoming kind must preserve another author's archived location row. It must
// never fall through to the message upsert. The caller holds the IMMEDIATE
// transaction across this check and any insert of a previously unknown key.
// A same-author update changes position only; out-of-order samples cannot go
// backwards. A distinct ID or chat remains a distinct archived row.
func updateLiveLocationWith(ex locationWriter, id, chat, sender string, fromMe bool, p *messageLocation) (bool, error) {
	if id == "" {
		return false, nil
	}
	user, server := splitSenderJID(sender)
	var sameAuthor, locationRow bool
	err := ex.QueryRow(`SELECT COALESCE(sender = ? AND sender_server IS ? AND is_from_me = ?, 0),
		COALESCE(media_type, '') = 'location' FROM messages WHERE id = ? AND chat_jid = ?`, user, server, fromMe, id, chat).Scan(&sameAuthor, &locationRow)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameAuthor && (locationRow || p != nil && p.Live) {
		return true, nil
	}
	if p == nil || !p.Live {
		return false, nil
	}
	if !p.update() {
		return false, nil // the initial sample may restore its original metadata
	}
	position := *p
	position.Name, position.Address, position.URL, position.Comment = "", "", "", ""
	_, err = ex.Exec(`UPDATE messages SET location = CASE
		WHEN COALESCE(json_extract(location, '$.sequence'), 0) < ? THEN json_patch(location, ?) ELSE location END
		WHERE id = ? AND chat_jid = ? AND media_type = 'location'
		AND sender = ? AND sender_server IS ? AND is_from_me = ?
		AND CASE WHEN json_valid(location) THEN json_extract(location, '$.live') = 1 ELSE 0 END`,
		*p.Sequence, position.column(), id, chat, user, server, fromMe)
	return true, err
}

func (s *MessageStore) UpdateLiveLocation(id, chat, sender string, fromMe bool, p *messageLocation) (bool, error) {
	if id == "" {
		return false, nil
	}
	handled := false
	err := s.Batch(func(b *messageBatch) error {
		var err error
		handled, err = b.UpdateLiveLocation(id, chat, sender, fromMe, p)
		return err
	})
	return handled, err
}

func (b *messageBatch) UpdateLiveLocation(id, chat, sender string, fromMe bool, p *messageLocation) (bool, error) {
	matched := false
	err := b.write(func() error {
		var err error
		matched, err = updateLiveLocationWith(b.tx, id, chat, sender, fromMe, p)
		return err
	})
	return matched, err
}

// A history position update is not fresh conversation activity. Only the
// location case changes the usual newest-first timestamp value; every own-phone
// chunk checks for a location-key collision under its existing write lock.
type locationSampleKey struct {
	id, sender string // sender is the full resolved JID used by persistence
	fromMe     bool
}

func (b *Bridge) historyLocationInitialTimes(rows []*waHistorySync.HistorySyncMsg, chat types.JID, fallback time.Time) map[locationSampleKey]time.Time {
	initial := make(map[locationSampleKey]time.Time)
	for _, row := range rows {
		info := row.GetMessage()
		p := extractMessage(info.GetMessage(), fallback, info.GetKey().GetID()).location
		if p != nil && p.Live && !p.update() && info.GetKey().GetID() != "" && info.GetMessageTimestamp() != 0 {
			stamp := time.Unix(int64(info.GetMessageTimestamp()), 0) //nolint:gosec // WhatsApp epoch seconds
			sender, fromMe := b.historySender(info, chat, false)
			key := locationSampleKey{id: info.GetKey().GetID(), sender: storedSender(sender), fromMe: fromMe}
			old, found := initial[key]
			if !found || stamp.Before(old) {
				initial[key] = stamp
			}
		}
	}
	return initial
}
