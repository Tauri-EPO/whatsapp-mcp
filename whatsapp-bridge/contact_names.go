package main

import (
	"context"
	"database/sql"
	"strings"
	"unicode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const contactNameBatchSize = 256

type localContactIdentity struct {
	JID, Name                      string
	phone                          string
	jid, twin                      types.JID
	original                       string
	needsName, resolved, forceName bool
}

func localContactJID(raw string) types.JID {
	jid, _ := types.ParseJID(raw)
	return jid.ToNonAD()
}

func contactNameFields(full, push, first, business string) []string {
	return []string{full, push, first, business}
}

// resolveLocalContactNames reads only local identities in bounded batches.
// Supplying a client requests SDK contact reads, which may populate its cache.
// Archived names are retained unless the caller needs a placeholder repaired.
func (store *MessageStore) resolveLocalContactNames(ctx context.Context, identities []localContactIdentity, account string, client *whatsmeow.Client, logger waLog.Logger) map[string]localContactIdentity {
	out := make(map[string]localContactIdentity, len(identities))
	unique := make([]localContactIdentity, 0, len(identities))
	seen := map[string]bool{}
	for _, identity := range identities {
		identity.original = identity.JID
		identity.jid = localContactJID(identity.JID)
		if seen[identity.original] || (identity.jid.Server != types.DefaultUserServer && identity.jid.Server != types.HiddenUserServer) {
			continue
		}
		seen[identity.original] = true
		identity.JID = identity.jid.String()
		if identity.jid.Server == types.DefaultUserServer {
			identity.phone = identity.jid.User
		}
		unique = append(unique, identity)
	}
	for start := 0; start < len(unique); start += contactNameBatchSize {
		batch := unique[start:min(start+contactNameBatchSize, len(unique))]
		sdkContacts := map[string]types.ContactInfo{}
		if client != nil && client.Store != nil && client.Store.Contacts != nil {
			for _, identity := range batch {
				if contact, err := client.Store.Contacts.GetContact(ctx, identity.jid); err == nil {
					sdkContacts[identity.original] = contact
				}
			}
		}
		store.readLocalContactNames(ctx, batch, account, logger)
		for _, identity := range batch {
			if identity.needsName {
				if contact, ok := sdkContacts[identity.original]; ok {
					// SDK FullName wins; other SDK fields only fill a SQL miss.
					fields := []string{contact.FullName}
					if !identity.resolved {
						fields = contactNameFields(contact.FullName, contact.PushName, contact.FirstName, contact.BusinessName)
					}
					for _, field := range fields {
						if value := strings.TrimSpace(field); value != "" && !isPlaceholderName(value, identity.jid, identity.twin) {
							identity.Name, identity.resolved = value, true
							break
						}
					}
				}
			}
			out[identity.original] = identity
		}
	}
	return out
}

func warnLocalContactQuery(logger waLog.Logger, table string, err error) {
	if err != nil && logger != nil && !strings.Contains(err.Error(), "no such table: "+table) {
		logger.Warnf("Failed to query local contact names (%s): %v", table, err)
	}
}

func (store *MessageStore) readLocalContactNames(ctx context.Context, chats []localContactIdentity, account string, logger waLog.Logger) {
	alternatives := map[string]types.JID{}
	if store != nil && store.waDB != nil && len(chats) > 0 {
		users := make([]any, 0, len(chats))
		for _, chat := range chats {
			users = append(users, chat.jid.User)
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(users)), ",")
		args := append(append([]any{}, users...), users...)
		lookupSQL := `SELECT lid,pn FROM whatsmeow_lid_map WHERE lid IN (` + marks + `) OR pn IN (` + marks + `)` //nolint:gosec // Only generated placeholders are concatenated; identities are bound.
		rows, err := store.waDB.QueryContext(ctx, lookupSQL, args...)
		if err == nil {
			for rows.Next() {
				var lid, phone string
				if err = rows.Scan(&lid, &phone); err != nil {
					break
				}
				lidJID, phoneJID := types.NewJID(lid, types.HiddenUserServer), types.NewJID(phone, types.DefaultUserServer)
				alternatives[lidJID.String()] = phoneJID
				alternatives[phoneJID.String()] = lidJID
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
		}
		warnLocalContactQuery(logger, "whatsmeow_lid_map", err)
	}
	args := []any{account}
	identities := map[string]types.JID{}
	for i := range chats {
		chat := &chats[i]
		chat.twin = alternatives[chat.JID]
		if chat.twin.Server == types.DefaultUserServer {
			chat.phone = chat.twin.User
		}
		chat.needsName = chat.forceName || isPlaceholderName(chat.Name, chat.jid, chat.twin)
		if chat.needsName {
			args = append(args, chat.JID)
			identities[chat.JID] = chat.jid
			if !chat.twin.IsEmpty() {
				args = append(args, chat.twin.String())
				identities[chat.twin.String()] = chat.twin
			}
		}
	}
	if store == nil || store.waDB == nil || len(args) == 1 || account == "" {
		return
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)-1), ",")
	lookupSQL := `SELECT their_jid,full_name,push_name,first_name,business_name FROM whatsmeow_contacts WHERE our_jid = ? AND their_jid IN (` + marks + `)` //nolint:gosec // Only generated placeholders are concatenated; identities are bound.
	rows, err := store.waDB.QueryContext(ctx, lookupSQL, args...)
	if err != nil {
		warnLocalContactQuery(logger, "whatsmeow_contacts", err)
		return
	}
	names := map[string]string{}
	for rows.Next() {
		var jid string
		var full, push, first, business sql.NullString
		if err = rows.Scan(&jid, &full, &push, &first, &business); err != nil {
			break
		}
		for _, field := range contactNameFields(full.String, push.String, first.String, business.String) {
			if value := strings.TrimSpace(field); value != "" && !isPlaceholderName(value, identities[jid], alternatives[jid]) {
				names[jid] = value
				break
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	warnLocalContactQuery(logger, "whatsmeow_contacts", err)
	for i := range chats {
		if chats[i].needsName {
			name := names[chats[i].JID]
			if name == "" {
				name = names[chats[i].twin.String()]
			}
			if name != "" {
				chats[i].Name, chats[i].resolved = name, true
			}
		}
	}
}

func localPhoneDigits(value string) string {
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

// Contact placeholders are direct phone/LID labels only: empty, or the
// formatted number of the identity or its known twin. Chat-name history
// fallbacks (groups and misplaced self labels) remain in chat_names.go.
func isPlaceholderName(name string, jid types.JID, twins ...types.JID) bool {
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
		return false
	}
	if strings.TrimSpace(name) == "" {
		return true
	}
	number := localPhoneDigits(name)
	if number == "" {
		return false
	}
	if number == jid.User {
		return true
	}
	for _, identity := range twins {
		if !identity.IsEmpty() && number == identity.User {
			return true
		}
	}
	return false
}
