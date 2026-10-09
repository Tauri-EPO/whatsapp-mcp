package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// action_ms is the app-state ordering key in milliseconds, not a displayed time.
// Canonical TIMESTAMP values still use dbTime; retaining this key prevents an
// older mutation in the same second from resurrecting a deletion.
const labelsSchema = `
CREATE TABLE IF NOT EXISTS labels (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, color INTEGER NOT NULL,
 deleted BOOLEAN NOT NULL, updated_at TIMESTAMP NOT NULL, action_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS chat_labels (
 chat_jid TEXT NOT NULL, label_id TEXT NOT NULL, labeled BOOLEAN NOT NULL,
 updated_at TIMESTAMP NOT NULL, action_ms INTEGER NOT NULL, PRIMARY KEY (chat_jid, label_id)
);
CREATE INDEX IF NOT EXISTS idx_chat_labels_label ON chat_labels(label_id);
`

func ensureLabelMetadata(db schemaWriter) error {
	for _, col := range []struct{ name, spec string }{
		{"type", "INTEGER NOT NULL DEFAULT 0"},
		{"immutable", "BOOLEAN NOT NULL DEFAULT 0"},
		{"predefined_id", "INTEGER"},
	} {
		if err := ensureColumn(db, "labels", col.name, col.spec); err != nil {
			return err
		}
	}
	return nil
}

type labelRecord struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Color        int32  `json:"color"`
	Deleted      bool   `json:"deleted"`
	Type         int32  `json:"type"`
	Immutable    bool   `json:"immutable"`
	PredefinedID *int32 `json:"predefined_id"`
}

func (s *MessageStore) storeLabel(e *events.LabelEdit) error {
	_, err := s.db.Exec(`INSERT INTO labels(id, name, color, deleted, updated_at, action_ms, type, immutable, predefined_id)
	 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET
	 name = CASE WHEN excluded.deleted AND excluded.name = '' THEN labels.name ELSE excluded.name END,
	 color = excluded.color, deleted = excluded.deleted, updated_at = excluded.updated_at, action_ms = excluded.action_ms,
	 type = excluded.type, immutable = excluded.immutable, predefined_id = excluded.predefined_id
	 WHERE excluded.action_ms > labels.action_ms OR
	 (excluded.action_ms = labels.action_ms AND (excluded.deleted OR NOT labels.deleted))`,
		e.LabelID, e.Action.GetName(), e.Action.GetColor(), e.Action.GetDeleted(), dbTime(e.Timestamp), e.Timestamp.UnixMilli(),
		int32(e.Action.GetType()), e.Action.GetIsImmutable(), e.Action.PredefinedID)
	return err
}

func (s *MessageStore) storeChatLabel(chat string, e *events.LabelAssociationChat) error {
	// Keep an unlabeled tombstone: an older full snapshot must not resurrect it.
	_, err := s.db.Exec(`INSERT INTO chat_labels(chat_jid, label_id, labeled, updated_at, action_ms)
	 VALUES(?, ?, ?, ?, ?) ON CONFLICT(chat_jid, label_id) DO UPDATE SET
	 labeled = excluded.labeled, updated_at = excluded.updated_at, action_ms = excluded.action_ms
	 WHERE excluded.action_ms > chat_labels.action_ms OR
	 (excluded.action_ms = chat_labels.action_ms AND (NOT excluded.labeled OR chat_labels.labeled))`,
		chat, e.LabelID, e.Action.GetLabeled(), dbTime(e.Timestamp), e.Timestamp.UnixMilli())
	return err
}

func (b *Bridge) recordLabel(e *events.LabelEdit) {
	if e.Action == nil || e.LabelID == "" {
		return
	}
	b.storeLive("label", e.LabelID, "", func() error { return b.Store.storeLabel(e) })
}

func (b *Bridge) recordChatLabel(e *events.LabelAssociationChat) {
	if e.Action == nil || e.LabelID == "" || e.JID.User == "" || e.JID.Server == "" {
		return
	}
	// Preserve the source namespace so a LID-only policy can read LID events.
	// Verified permitted twins are merged at read time, without losing tombstones.
	chat := e.JID.ToNonAD().String()
	b.storeLive("chat label", e.LabelID, chat, func() error { return b.Store.storeChatLabel(chat, e) })
}

func (s *MessageStore) listLabels(chats []string, includeDeleted bool) ([]labelRecord, error) {
	// At most the requested chat and its one verified twin. Keep SQL constant;
	// neither identifiers nor values need interpolation.
	var first, second string
	if len(chats) > 0 {
		first = chats[0]
	}
	if len(chats) > 1 {
		second = chats[1]
	}
	rows, err := s.db.Query(`SELECT id, name, color, deleted, type, immutable, predefined_id FROM labels l
	 WHERE (NOT deleted OR ?) AND (? OR
	 (SELECT c.labeled FROM chat_labels c WHERE c.label_id=l.id AND c.chat_jid IN (?, ?)
	 ORDER BY c.action_ms DESC, c.labeled ASC LIMIT 1)=1) ORDER BY id`, includeDeleted, len(chats) == 0, first, second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	labels := []labelRecord{}
	for rows.Next() {
		var label labelRecord
		var predefined sql.NullInt32
		if err := rows.Scan(&label.ID, &label.Name, &label.Color, &label.Deleted, &label.Type, &label.Immutable, &predefined); err != nil {
			return nil, err
		}
		if predefined.Valid {
			label.PredefinedID = &predefined.Int32
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func handleLabels(s *MessageStore, policy chatPolicy, twin func(context.Context, types.JID) (types.JID, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat, hasChat := r.URL.Query()["chat_jid"]
		var chats []string
		if hasChat {
			jid, ok := authorizeChat(w, policy, chat[0], false)
			if !ok {
				return
			}
			chats = append(chats, jid.String())
			if twin != nil {
				if other, err := twin(r.Context(), jid); err == nil && !other.IsEmpty() && other.String() != jid.String() && policy.Allows(other.String()) {
					chats = append(chats, other.String())
				}
			}
		}
		include := r.URL.Query().Get("include_deleted")
		if include != "" && include != "0" && include != "1" {
			writeError(w, 400, "include_deleted must be 0 or 1")
			return
		}
		labels, err := s.listLabels(chats, include == "1")
		if err != nil {
			writeError(w, 500, "Cannot read label cache: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"labels": labels})
	}
}

type labelDeps struct {
	store     *MessageStore
	policy    chatPolicy
	connected func() bool
	resolve   func(context.Context, string) (types.JID, error)
	send      appStateSendFunc
}

func handleLabelChat(deps labelDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := requestContext(r, archiveDeadline)
		defer cancel()
		var req struct {
			ChatJID string `json:"chat_jid"`
			LabelID string `json:"label_id"`
			Labeled *bool  `json:"labeled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "Invalid request format")
			return
		}
		req.ChatJID, req.LabelID = strings.TrimSpace(req.ChatJID), strings.TrimSpace(req.LabelID)
		if req.LabelID == "" || req.Labeled == nil {
			writeError(w, 400, "Valid chat_jid, label_id and labeled boolean are required")
			return
		}
		chat, ok := authorizeChat(w, deps.policy, req.ChatJID, false)
		if !ok {
			return
		}
		req.ChatJID = chat.String()
		if chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer && chat.Server != types.GroupServer {
			writeError(w, 400, "Only direct chats and groups can be labelled")
			return
		}
		if !deps.connected() {
			writeError(w, 503, "WhatsApp client is not connected. Please wait for reconnection.")
			return
		}
		var deleted, immutable bool
		err := deps.store.db.QueryRowContext(ctx, "SELECT deleted, immutable FROM labels WHERE id=?", req.LabelID).Scan(&deleted, &immutable)
		if ctx.Err() != nil {
			writeErrorCode(w, 408, "bridge_unavailable", "Label request expired before send; nothing sent; safe to retry")
			return
		}
		if errors.Is(err, sql.ErrNoRows) || (err == nil && deleted) {
			writeError(w, 404, "Unknown or deleted label")
			return
		}
		if err != nil {
			writeError(w, 500, "Cannot read label cache: "+err.Error())
			return
		}
		if immutable {
			writeError(w, 400, "Label is immutable and cannot be applied or removed")
			return
		}
		target, err := deps.resolve(ctx, req.ChatJID)
		if ctx.Err() != nil {
			writeErrorCode(w, 408, "bridge_unavailable", "Label request expired before send; nothing sent; safe to retry")
			return
		}
		if err != nil || target.User == "" || target.Server == "" {
			writeError(w, 400, "Cannot resolve chat_jid")
			return
		}
		writeAppStateResult(w, deps.send(ctx, appstate.BuildLabelChat(target, req.LabelID, *req.Labeled)),
			map[string]any{"label_id": req.LabelID, "labeled": *req.Labeled}, "Chat label")
	}
}

// Connected only schedules this work. Once and the wait group belong to this
// Bridge, and Shutdown seals Once before cancelling and waiting for it.
func (b *Bridge) startLabelSync() {
	if b.LabelResync == nil || !b.isPaired() || !b.Connected() {
		return
	}
	b.labelSyncOnce.Do(func() {
		b.labelSyncWait.Add(1)
		fetch, timeout := b.LabelResync, b.LabelResyncTimeout
		if timeout <= 0 {
			timeout = actionDeadline
		}
		go func() {
			defer b.labelSyncWait.Done()
			b.clientGate.RLock()
			defer b.clientGate.RUnlock()
			if b.operatorLogout.Load() {
				return
			}
			ctx, cancel := context.WithTimeout(b.ctx, timeout)
			defer cancel()
			// Recheck eligibility after waiting for an outbound writer: the cache
			// or socket may have changed while this goroutine queued.
			err := b.withAppStateWriter(ctx, func(ctx context.Context) error {
				var populated bool
				if err := b.Store.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM labels LIMIT 1)").Scan(&populated); err != nil {
					return err
				}
				if populated || !b.isPaired() || !b.Connected() || ctx.Err() != nil {
					return nil
				}
				b.Log.Infof("Empty label cache: requesting one regular app-state snapshot")
				err := fetch(ctx)
				if err == nil {
					b.Log.Infof("Startup label sync completed")
				}
				return err
			})
			if err != nil && b.ctx.Err() == nil {
				b.Log.Warnf("Startup label sync failed: %v", err)
			}
		}()
	})
}
