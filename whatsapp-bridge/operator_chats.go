package main

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Explicit identity-only projection: no message or media columns are read.
type operatorChat struct {
	JID             string     `json:"jid"`
	Name            string     `json:"name"`
	IsGroup         bool       `json:"is_group"`
	LastMessageTime *time.Time `json:"last_message_time"`
	phone           string
}

// operatorFold is a case-insensitive, composition-insensitive comparison key:
// "José" typed precomposed matches a name stored decomposed.
func operatorFold(value string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

// A Brazilian mobile may be archived with or without its ninth digit; either
// spelling of the query finds either spelling of the chat.
func operatorChatMatches(chat operatorChat, query, digits string) bool {
	if strings.Contains(operatorFold(chat.Name), query) {
		return true
	}
	if digits == "" || chat.phone == "" {
		return false
	}
	alternate := brMobileAlternate(digits)
	for _, phone := range []string{chat.phone, brMobileAlternate(chat.phone)} {
		if phone != "" && (strings.Contains(phone, digits) || alternate != "" && strings.Contains(phone, alternate)) {
			return true
		}
	}
	return false
}

// operatorChatRows copies the candidate identities into memory and closes the
// cursor before any contact lookup, so a search holds one of the few
// messages.db connections only for this scan. maxRows 0 reads every row.
func (store *MessageStore) operatorChatRows(ctx context.Context, deploy chatPolicy, query string, maxRows int) ([]operatorChat, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT jid, COALESCE(name, ''), CAST(last_message_time AS TEXT)
		FROM chats ORDER BY last_message_time DESC, jid ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var chats []operatorChat
	for rows.Next() {
		var chat operatorChat
		var stamp sql.NullString
		if err := rows.Scan(&chat.JID, &chat.Name, &stamp); err != nil {
			return nil, err
		}
		if !deploy.Allows(chat.JID) {
			continue
		}
		chat.IsGroup = strings.HasSuffix(chat.JID, "@g.us")
		user, server, _ := strings.Cut(chat.JID, "@")
		// A blank name shows the JID user; for a direct chat that is still a
		// placeholder the contact lookup may replace.
		if chat.Name = strings.TrimSpace(chat.Name); chat.Name == "" {
			chat.Name = user
		}
		direct := server == "s.whatsapp.net" || server == "lid"
		// Only a placeholder name can change through contacts, and only a
		// direct chat has digits to match: skip rows that can never match.
		if query != "" && !direct && !strings.Contains(operatorFold(chat.Name), query) {
			continue
		}
		if stamp.Valid {
			// Migrations deliberately leave unreadable legacy values: unknown.
			if parsed, err := parseDBTime(stamp.String); err == nil {
				chat.LastMessageTime = &parsed
			}
		}
		chats = append(chats, chat)
		if len(chats) == maxRows {
			break
		}
	}
	return chats, rows.Err()
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
	withDeadline := b.operatorChatsDeadline
	if withDeadline == nil {
		withDeadline = context.WithTimeout
	}
	ctx, cancel := withDeadline(r.Context(), 5*time.Second)
	defer cancel()
	account := ""
	// The SDK mutates client.Store.ID during pairing and logout; read the
	// bridge's atomic snapshot instead.
	if id := b.runtimeDeviceID.Load(); id != nil && b.isPaired() {
		account = *id
	} else if b.Store.waDB != nil {
		// Offline archives can still carry a phone book. Select its account once
		// so every per-identity lookup can use the (our_jid, their_jid) key.
		_ = b.Store.waDB.QueryRowContext(ctx, `SELECT our_jid FROM whatsmeow_contacts ORDER BY our_jid LIMIT 1`).Scan(&account)
	}
	// Ignore only the runtime chats.allowed layer: it can only narrow the
	// deploy WHATSAPP_ALLOWED_CHATS, so a chat outside the deploy list could
	// never be allowed and stays outside this search too.
	deploy := deploymentChatPolicy(b.Policy)
	query := operatorFold(strings.TrimSpace(r.URL.Query().Get("query")))
	digits := localPhoneDigits(query)
	// Without a query every row matches, so read and resolve only the page
	// asked for. With one, resolve names in pages of 256 identities (at most
	// 513 bound parameters, even with every alias).
	page, maxRows := 256, 0
	if query == "" {
		page, maxRows = limit, limit
	}
	candidates, err := b.Store.operatorChatRows(ctx, deploy, query, maxRows)
	if err != nil {
		writeError(w, 503, "Chat archive unavailable")
		return
	}
	chats := make([]operatorChat, 0, 50)
	for start := 0; start < len(candidates) && len(chats) < limit; start += page {
		batch := candidates[start:min(start+page, len(candidates))]
		identities := make([]localContactIdentity, 0, len(batch))
		for _, chat := range batch {
			if strings.TrimSpace(chat.Name) == "" || localPhoneDigits(chat.Name) != "" || digits != "" && strings.HasSuffix(chat.JID, "@lid") {
				identities = append(identities, localContactIdentity{JID: chat.JID, Name: chat.Name})
			}
		}
		var names map[string]localContactIdentity
		if b.operatorChatsResolveNames != nil {
			names = b.operatorChatsResolveNames(ctx, identities, account)
		} else {
			names = b.Store.resolveLocalContactNames(ctx, identities, account, nil, b.Log)
		}
		for i := range batch {
			jid := localContactJID(batch[i].JID)
			if jid.Server == "s.whatsapp.net" {
				batch[i].phone = jid.User
			}
			if contact, ok := names[batch[i].JID]; ok {
				if contact.phone != "" {
					batch[i].phone = contact.phone
				}
				if contact.resolved {
					batch[i].Name = contact.Name
				}
			}
		}
		for _, chat := range batch {
			if query != "" && !operatorChatMatches(chat, query, digits) {
				continue
			}
			chats = append(chats, chat)
			if len(chats) == limit {
				break
			}
		}
	}
	if ctx.Err() != nil {
		writeError(w, 503, "Chat archive unavailable")
		return
	}
	if out, ok := w.(*operatorStatusWriter); ok {
		out.results = len(chats)
	}
	writeJSON(w, 200, map[string]any{"chats": chats})
}
