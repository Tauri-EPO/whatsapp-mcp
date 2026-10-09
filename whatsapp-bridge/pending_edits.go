package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

const (
	pendingEditTTL       = 24 * time.Hour
	pendingEditGlobalCap = 1024
	pendingEditChatCap   = 128
	pendingEditMaxBytes  = 64 << 10
)

// No chat FK: an edit alone must not create an archive chat or message.
// Arrival/expiry are epoch milliseconds, independent of the protocol timestamp.
const pendingEditsSchema = `CREATE TABLE IF NOT EXISTS pending_edits (
	chat_jid TEXT NOT NULL, target_id TEXT NOT NULL,
	sender TEXT NOT NULL, sender_server TEXT NOT NULL, is_from_me BOOLEAN NOT NULL,
	edit_timestamp INTEGER NOT NULL, content TEXT NOT NULL, mentions TEXT NOT NULL,
	arrived_ms INTEGER NOT NULL, expires_ms INTEGER NOT NULL,
	PRIMARY KEY(chat_jid, target_id, sender, sender_server, is_from_me)
);
CREATE INDEX IF NOT EXISTS idx_pending_edits_expiry ON pending_edits(expires_ms);
CREATE INDEX IF NOT EXISTS idx_pending_edits_target ON pending_edits(target_id);
CREATE INDEX IF NOT EXISTS idx_pending_edits_arrival ON pending_edits(arrived_ms);
CREATE INDEX IF NOT EXISTS idx_pending_edits_chat_arrival ON pending_edits(chat_jid,arrived_ms);`

func (s *MessageStore) pendingEditNow() time.Time {
	if s.editNow != nil {
		return s.editNow()
	}
	return time.Now()
}

func prunePendingEdits(ex sqlExecer, now time.Time) error {
	_, err := ex.Exec("DELETE FROM pending_edits WHERE expires_ms <= ?", now.UnixMilli())
	return err
}

func applyOrDeferMessageEdit(ex locationWriter, chat, sender string, own bool, edit *waE2E.ProtocolMessage, fallback, now time.Time) error {
	if chat == "" || edit.GetKey().GetID() == "" || edit.GetEditedMessage() == nil {
		return nil
	}
	if err := prunePendingEdits(ex, now); err != nil {
		return err
	}
	var exists bool
	if err := ex.QueryRow("SELECT EXISTS(SELECT 1 FROM messages WHERE id=? AND chat_jid=?)", edit.GetKey().GetID(), chat).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return applyMessageEditWith(ex, chat, sender, own, edit, fallback)
	}
	jid, err := types.ParseJID(sender)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
		return nil // an unattributed/group author cannot authorize a later edit
	}
	stamp := edit.GetTimestampMS()
	if stamp <= 0 {
		stamp = fallback.UnixMilli()
	}
	if stamp <= 0 {
		return nil
	}
	replacement := extractMessage(edit.GetEditedMessage(), fallback, edit.GetKey().GetID())
	mentions := mentionsColumn(replacement.mentions)
	if len(chat)+len(sender)+len(edit.GetKey().GetID())+len(replacement.content)+len(mentions) > pendingEditMaxBytes {
		return nil // bounded pending payloads; never truncate an eventual edit
	}
	_, err = ex.Exec(`INSERT INTO pending_edits
		(chat_jid,target_id,sender,sender_server,is_from_me,edit_timestamp,content,mentions,arrived_ms,expires_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(chat_jid,target_id,sender,sender_server,is_from_me)
		DO UPDATE SET edit_timestamp=excluded.edit_timestamp,content=excluded.content,mentions=excluded.mentions
		WHERE excluded.edit_timestamp > pending_edits.edit_timestamp`, chat, edit.GetKey().GetID(), jid.User, jid.Server, own,
		stamp, replacement.content, mentions, now.UnixMilli(), now.Add(pendingEditTTL).UnixMilli())
	if err != nil {
		return err
	}
	// Keep newest arrivals; replay/newer versions never refresh the first TTL.
	return capPendingEdits(ex, chat)
}

func capPendingEdits(ex sqlExecer, chat string) error {
	if _, err := ex.Exec(`DELETE FROM pending_edits WHERE rowid IN (
		SELECT rowid FROM pending_edits WHERE chat_jid=? ORDER BY arrived_ms DESC,rowid DESC LIMIT -1 OFFSET ?)`, chat, pendingEditChatCap); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM pending_edits WHERE rowid IN (
		SELECT rowid FROM pending_edits ORDER BY arrived_ms DESC,rowid DESC LIMIT -1 OFFSET ?)`, pendingEditGlobalCap)
	return err
}

// Resolve only a verified alternative author, before acquiring the archive writer.
// No cursor or SDK read survives into consumption's transaction.
func (b *Bridge) pendingEditAlias(ctx context.Context, id, chat, sender string, own bool) (string, string, error) {
	author, err := types.ParseJID(sender)
	if err != nil || author.User == "" || (author.Server != types.DefaultUserServer && author.Server != types.HiddenUserServer) {
		return "", "", nil
	}
	opposite := types.HiddenUserServer
	if author.Server == types.HiddenUserServer {
		opposite = types.DefaultUserServer
	}
	type pendingKey struct{ chat, user, server string }
	rows, err := b.Store.db.QueryContext(ctx, `SELECT chat_jid,sender,sender_server FROM pending_edits WHERE target_id=? AND is_from_me=? AND expires_ms>?`, id, own, b.Store.pendingEditNow().UnixMilli())
	if err != nil {
		return "", "", err
	}
	var candidates []pendingKey
	for rows.Next() {
		var key pendingKey
		if err := rows.Scan(&key.chat, &key.user, &key.server); err != nil {
			_ = rows.Close()
			return "", "", err
		}
		if (key.server == author.Server && key.user == author.User) || key.server == opposite {
			candidates = append(candidates, key)
		}
	}
	err = rows.Err()
	_ = rows.Close() // SDK reads below hold neither a cursor nor an archive writer
	if err != nil || len(candidates) == 0 {
		return "", "", err
	}
	chatJID, _ := types.ParseJID(chat)
	chatAlias := ""
	for _, key := range candidates {
		other, _ := types.ParseJID(key.chat)
		if key.chat != chat && ((chatJID.Server == types.DefaultUserServer && other.Server == types.HiddenUserServer) ||
			(chatJID.Server == types.HiddenUserServer && other.Server == types.DefaultUserServer)) {
			chatAlias, err = b.pendingEditAlt(ctx, chat)
			if err != nil {
				if ctx.Err() != nil {
					return "", "", ctx.Err()
				}
				// An unverified candidate in another DM must not make the
				// original depend on session availability. Retain that edit.
				chatAlias = ""
			}
			break
		}
	}
	// An equal message ID in another group/channel is never a session lookup.
	// Only the exact chat or its verified DM alias can need author resolution.
	for _, key := range candidates {
		if key.chat != chat && (chatAlias == "" || key.chat != chatAlias) {
			continue
		}
		if key.server == opposite {
			if sender == chat && chatAlias != "" {
				return chatAlias, chatAlias, nil
			}
			alias, err := b.pendingEditAlt(ctx, sender)
			if sender == chat && chatAlias == "" {
				chatAlias = alias
			}
			return alias, chatAlias, err
		}
	}
	return "", chatAlias, nil
}

func (b *Bridge) pendingEditAlt(ctx context.Context, sender string) (string, error) {
	jid, err := types.ParseJID(sender)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
		return "", nil
	}
	alt, err := lookupAltJID(ctx, b.currentClient(), jid.ToNonAD())
	if errors.Is(err, errLIDStoreUnavailable) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if alt.User != "" && ((jid.Server == types.DefaultUserServer && alt.Server == types.HiddenUserServer) ||
		(jid.Server == types.HiddenUserServer && alt.Server == types.DefaultUserServer)) {
		return alt.ToNonAD().String(), nil
	}
	return "", nil
}

func consumePendingEdits(ex locationWriter, message storedMessage, now time.Time) error {
	if err := prunePendingEdits(ex, now); err != nil {
		return err
	}
	user, server := splitSenderJID(message.Sender)
	aliasUser, aliasServer := splitSenderJID(message.EditAuthorAlias)
	const candidate = `SELECT p.%s FROM pending_edits p WHERE (p.chat_jid=messages.chat_jid OR (:chat_alias<>'' AND p.chat_jid=:chat_alias)) AND p.target_id=messages.id
		AND p.is_from_me=messages.is_from_me AND ((p.sender=messages.sender AND p.sender_server IS messages.sender_server)
		OR (p.sender=:alias_user AND p.sender_server IS :alias_server))
		ORDER BY p.edit_timestamp DESC LIMIT 1`
	// All three fields select the same newest candidate. FTS triggers and the
	// pending deletion participate in the original message's transaction.
	_, err := ex.Exec(`UPDATE messages SET
		content=(`+fmt.Sprintf(candidate, "content")+`),
		mentions=(`+fmt.Sprintf(candidate, "mentions")+`),
		message_edit_timestamp=(`+fmt.Sprintf(candidate, "edit_timestamp")+`)
		WHERE id=:id AND chat_jid=:chat AND sender=:sender AND sender_server IS :server
		AND is_from_me=:own AND deleted_at IS NULL
		AND message_edit_timestamp < (`+fmt.Sprintf(candidate, "edit_timestamp")+`)`,
		sql.Named("id", message.ID), sql.Named("chat", message.ChatJID), sql.Named("sender", user), sql.Named("server", server),
		sql.Named("own", message.IsFromMe), sql.Named("alias_user", aliasUser), sql.Named("alias_server", aliasServer), sql.Named("chat_alias", message.EditChatAlias))
	if err != nil {
		return err
	}
	_, err = ex.Exec("DELETE FROM pending_edits WHERE target_id=? AND (chat_jid=? OR (?<>'' AND chat_jid=?))", message.ID, message.ChatJID, message.EditChatAlias, message.EditChatAlias)
	return err
}

// Startup uses verified session mappings too, without attaching/reading that
// database inside the pending-edits archive writer. Drain both readers first.
func (s *MessageStore) migratePendingEditChats(ctx context.Context, sessionPath string) error {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT chat_jid FROM pending_edits WHERE chat_jid LIKE '%@lid' ORDER BY chat_jid")
	if err != nil {
		return err
	}
	var chats []string
	for rows.Next() {
		var chat string
		if err := rows.Scan(&chat); err != nil {
			_ = rows.Close()
			return err
		}
		chats = append(chats, chat)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(chats) == 0 {
		return err
	}
	session, err := openWhatsmeowContactsDB(sessionPath)
	if err != nil || session == nil {
		return err
	}
	defer func() { _ = session.Close() }()
	var table bool
	if err := session.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='whatsmeow_lid_map')").Scan(&table); err != nil || !table {
		return err
	}
	prepared := make(map[string]string)
	for _, chat := range chats {
		jid, err := types.ParseJID(chat)
		if err != nil {
			continue
		}
		var phone string
		err = session.QueryRowContext(ctx, "SELECT pn FROM whatsmeow_lid_map WHERE lid=?", jid.User).Scan(&phone)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if isPhoneDigits(phone) {
			prepared[chat] = types.NewJID(phone, types.DefaultUserServer).String()
		}
	}
	return s.BatchContext(ctx, func(batch *messageBatch) error {
		return batch.write(func() error {
			if err := prunePendingEdits(batch.tx, s.pendingEditNow()); err != nil {
				return err
			}
			for _, chat := range chats {
				phone, ok := prepared[chat]
				if !ok {
					continue
				}
				if _, err := batch.tx.Exec(`INSERT INTO pending_edits
					(chat_jid,target_id,sender,sender_server,is_from_me,edit_timestamp,content,mentions,arrived_ms,expires_ms)
					SELECT ?,target_id,sender,sender_server,is_from_me,edit_timestamp,content,mentions,arrived_ms,expires_ms
					FROM pending_edits WHERE chat_jid=?
					ON CONFLICT(chat_jid,target_id,sender,sender_server,is_from_me) DO UPDATE SET
					content=CASE WHEN excluded.edit_timestamp>pending_edits.edit_timestamp THEN excluded.content ELSE pending_edits.content END,
					mentions=CASE WHEN excluded.edit_timestamp>pending_edits.edit_timestamp THEN excluded.mentions ELSE pending_edits.mentions END,
					edit_timestamp=MAX(pending_edits.edit_timestamp,excluded.edit_timestamp),
					arrived_ms=MIN(pending_edits.arrived_ms,excluded.arrived_ms),expires_ms=MIN(pending_edits.expires_ms,excluded.expires_ms)`, phone, chat); err != nil {
					return err
				}
				if _, err := batch.tx.Exec("DELETE FROM pending_edits WHERE chat_jid=?", chat); err != nil {
					return err
				}
				if err := capPendingEdits(batch.tx, phone); err != nil {
					return err
				}
			}
			return nil
		})
	})
}
