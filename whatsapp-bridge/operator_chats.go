package main

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Explicit identity-only projection: no message or media columns are read.
type operatorChat struct {
	JID             string     `json:"jid"`
	Name            string     `json:"name"`
	IsGroup         bool       `json:"is_group"`
	LastMessageTime *time.Time `json:"last_message_time"`
	phone           string
}

func operatorPhoneDigits(value string) string {
	var digits strings.Builder
	for _, ch := range value {
		if ch >= '0' && ch <= '9' {
			digits.WriteRune(ch)
		} else if !unicode.IsSpace(ch) && !strings.ContainsRune("+()-.", ch) {
			return ""
		}
	}
	return digits.String()
}

func operatorPlaceholderName(name string) bool {
	return strings.TrimSpace(name) == "" || operatorPhoneDigits(name) != ""
}

// Optional local phone book only; never call WhatsApp, warm name caches or log
// contact values. A missing/unavailable phone book leaves the archived name.
func (store *MessageStore) operatorContacts(ctx context.Context, chats []operatorChat, account string, needPhone bool) {
	var users []any
	for i := range chats {
		user, server, _ := strings.Cut(chats[i].JID, "@")
		if server == "s.whatsapp.net" {
			chats[i].phone = user
		}
		if (server == "s.whatsapp.net" || server == "lid") && (operatorPlaceholderName(chats[i].Name) || needPhone && server == "lid") {
			users = append(users, user)
		}
	}
	if store.waDB == nil || len(users) == 0 {
		return
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(users)), ",")
	alternatives := map[string]string{}
	args := append(append([]any{}, users...), users...)
	lookupSQL := `SELECT lid,pn FROM whatsmeow_lid_map WHERE lid IN (` + marks + `) OR pn IN (` + marks + `)` //nolint:gosec // Only generated placeholders are concatenated; identities are bound.
	rows, err := store.waDB.QueryContext(ctx, lookupSQL, args...)
	if err == nil {
		for rows.Next() {
			var lid, phone string
			if rows.Scan(&lid, &phone) == nil {
				alternatives[lid+"@lid"] = phone + "@s.whatsapp.net"
				alternatives[phone+"@s.whatsapp.net"] = lid + "@lid"
			}
		}
		_ = rows.Close()
	}
	args = []any{account}
	for i := range chats {
		if alternative := alternatives[chats[i].JID]; strings.HasSuffix(alternative, "@s.whatsapp.net") {
			chats[i].phone, _, _ = strings.Cut(alternative, "@")
		}
		if operatorPlaceholderName(chats[i].Name) && (strings.HasSuffix(chats[i].JID, "@lid") || strings.HasSuffix(chats[i].JID, "@s.whatsapp.net")) {
			args = append(args, chats[i].JID)
			if alternative := alternatives[chats[i].JID]; alternative != "" {
				args = append(args, alternative)
			}
		}
	}
	if len(args) == 1 || account == "" {
		return
	}
	marks = strings.TrimSuffix(strings.Repeat("?,", len(args)-1), ",")
	lookupSQL = `SELECT their_jid,full_name,push_name,first_name,business_name
		FROM whatsmeow_contacts WHERE our_jid = ? AND their_jid IN (` + marks + `)`
	rows, err = store.waDB.QueryContext(ctx, lookupSQL, args...)
	if err != nil {
		return
	}
	names := map[string]string{}
	for rows.Next() {
		var jid string
		var full, push, first, business sql.NullString
		if rows.Scan(&jid, &full, &push, &first, &business) == nil {
			for _, value := range []sql.NullString{full, push, first, business} {
				if !operatorPlaceholderName(value.String) {
					names[jid] = strings.TrimSpace(value.String)
					break
				}
			}
		}
	}
	_ = rows.Close()
	for i := range chats {
		if operatorPlaceholderName(chats[i].Name) {
			name := names[chats[i].JID]
			if name == "" {
				name = names[alternatives[chats[i].JID]]
			}
			if name != "" {
				chats[i].Name = name
			}
		}
	}
}

func (b *Bridge) handleOperatorChats(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if values, ok := r.URL.Query()["limit"]; ok {
		var err error
		if len(values) != 1 {
			writeError(w, 400, "limit must be 1-50")
			return
		}
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 50 {
			writeError(w, 400, "limit must be 1-50")
			return
		}
	}
	if b.Store == nil || b.Store.db == nil {
		writeError(w, 503, "Chat archive unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	account := ""
	if client := b.currentClient(); client != nil && client.Store != nil && client.Store.ID != nil {
		account = client.Store.ID.String()
	} else if b.Store.waDB != nil {
		// Offline archives can still carry a phone book. Select its account once
		// so every per-identity lookup can use the (our_jid, their_jid) key.
		_ = b.Store.waDB.QueryRowContext(ctx, `SELECT our_jid FROM whatsmeow_contacts ORDER BY our_jid LIMIT 1`).Scan(&account)
	}
	// Intentionally bypass both deploy and runtime allow-lists: the operator
	// uses these identities to choose which chats an assistant may access.
	rows, err := b.Store.db.QueryContext(ctx, `SELECT jid, COALESCE(name, ''), CAST(last_message_time AS TEXT)
		FROM chats ORDER BY last_message_time DESC, jid ASC`)
	if err != nil {
		writeError(w, 503, "Chat archive unavailable")
		return
	}
	defer func() { _ = rows.Close() }()
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	digits := operatorPhoneDigits(query)
	chats := make([]operatorChat, 0, limit)
	for len(chats) < limit {
		// Keep both archive and contact working sets bounded independently of
		// archive size. At most 513 bound parameters, even with every alias.
		page := make([]operatorChat, 0, 256)
		for len(page) < 256 && rows.Next() {
			var chat operatorChat
			var stamp sql.NullString
			if err := rows.Scan(&chat.JID, &chat.Name, &stamp); err != nil {
				writeError(w, 503, "Chat archive unavailable")
				return
			}
			chat.IsGroup = strings.HasSuffix(chat.JID, "@g.us")
			if stamp.Valid {
				parsed, err := parseDBTime(stamp.String)
				if err != nil {
					writeError(w, 503, "Chat archive unavailable")
					return
				}
				chat.LastMessageTime = &parsed
			}
			page = append(page, chat)
		}
		if len(page) == 0 {
			break
		}
		b.Store.operatorContacts(ctx, page, account, digits != "")
		for _, chat := range page {
			if chat.Name == "" {
				chat.Name, _, _ = strings.Cut(chat.JID, "@")
			}
			if query != "" && !strings.Contains(strings.ToLower(chat.Name), query) && (digits == "" || !strings.Contains(chat.phone, digits)) {
				continue
			}
			chats = append(chats, chat)
			if len(chats) == limit {
				break
			}
		}
	}
	if rows.Err() != nil || ctx.Err() != nil {
		writeError(w, 503, "Chat archive unavailable")
		return
	}
	if out, ok := w.(*operatorStatusWriter); ok {
		out.results = len(chats)
	}
	writeJSON(w, 200, map[string]any{"chats": chats})
}
