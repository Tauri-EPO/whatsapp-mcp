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

// A true result consumes a known key, including an author collision. It must
// never fall through to the message upsert. The caller holds the IMMEDIATE
// transaction across this check and any insert of a previously unknown key.
// A same-author update changes position only; out-of-order samples cannot go
// backwards. A distinct ID or chat remains a distinct archived row.
func updateLiveLocationWith(ex locationWriter, id, chat, sender string, fromMe bool, p *messageLocation) (bool, error) {
	if id == "" || p == nil || !p.Live {
		return false, nil
	}
	user, server := splitSenderJID(sender)
	var sameAuthor bool
	err := ex.QueryRow(`SELECT sender = ? AND sender_server IS ? AND is_from_me = ?
		FROM messages WHERE id = ? AND chat_jid = ?`, user, server, fromMe, id, chat).Scan(&sameAuthor)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameAuthor {
		return true, nil
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
	if id == "" || p == nil || !p.Live {
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
// location case changes the usual newest-first timestamp rule; other history
// chunks keep their existing marker and retry behavior.
func historyLocationInitialTimes(rows []*waHistorySync.HistorySyncMsg, fallback time.Time) map[string]time.Time {
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
	return initial
}

// The reader owns the current bounded chunk's IMMEDIATE transaction. Initial
// timestamps were collected without a write lock to support later chunks.
func (b *Bridge) historyLocationActivityTime(ctx context.Context, reader locationAuthorReader, rows []*waHistorySync.HistorySyncMsg, chat string, fallback time.Time, initial map[string]time.Time) (time.Time, error) {
	var latest time.Time
	for _, row := range rows {
		info := row.GetMessage()
		if info.GetMessageTimestamp() == 0 {
			continue
		}
		stamp := time.Unix(int64(info.GetMessageTimestamp()), 0) //nolint:gosec // same WhatsApp seconds as the canonical importer
		position := extractMessage(info.GetMessage(), fallback, info.GetKey().GetID()).location
		if position != nil && position.Live && info.GetKey().GetID() != "" {
			var original time.Time
			var user string
			var server sql.NullString
			var own bool
			err := reader.QueryRowContext(ctx, `SELECT timestamp,sender,sender_server,is_from_me FROM messages WHERE id=? AND chat_jid=?`, info.GetKey().GetID(), chat).Scan(&original, &user, &server, &own)
			if err == nil {
				// Positive samples never create activity for a known key. Initial
				// samples may restore metadata only for the same verified author.
				accepted := false
				if !position.update() {
					jid, _ := types.ParseJID(chat)
					sender, fromMe := b.historySender(info, jid, false)
					candidate, lookupErr := b.liveLocationSender(ctx, reader, info.GetKey().GetID(), chat, storedSender(sender), fromMe)
					incomingUser, incomingServer := splitSenderJID(candidate)
					accepted = lookupErr == nil && incomingUser == user && own == fromMe && ((server.Valid && incomingServer == server.String) || (!server.Valid && incomingServer == nil))
				}
				if !accepted {
					stamp = original
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return time.Time{}, err
			} else if position.update() {
				if first, found := initial[info.GetKey().GetID()]; found {
					stamp = first
				}
			}
		}
		if stamp.After(latest) {
			latest = stamp
		}
	}
	if latest.IsZero() {
		return fallback, nil
	}
	return latest, nil
}
