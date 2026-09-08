package main

// Which namespace a stored sender belongs to.
//
// messages.sender holds the bare user part of whoever sent a row. Until
// messages.sender_server existed nothing recorded which namespace that number
// lives in, and the two are indistinguishable by shape: a phone number
// (@s.whatsapp.net) and an anonymous link-ID (@lid) are both digits, and a
// 15-digit LID looks exactly like a (theoretically valid) 15-digit E.164
// number. The MCP server had to guess and got it wrong for the unresolved
// LIDs that status broadcasts are full of (issue #375). The bridge does not
// have to guess: msg.Info.Sender.Server says it at write time.
//
// The column is written on every insert (splitSenderJID below). NULL means
// "unknown", not "phone": rows an older bridge wrote carry it, and so does
// anything whose sender is not a user JID at all — the group JID a history
// sync falls back to when it names no participant, a newsletter or Messenger
// address. MigrateSenderNamespaces fills in what can still be established for
// the legacy rows, and the MCP server keeps its heuristic for whatever is left.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// splitSenderJID splits the sender a caller passes to StoreMessage into the
// bare user part stored in messages.sender and the namespace stored in
// messages.sender_server. Callers may pass either form: every bridge write
// path passes the full resolved JID, while a bare user part (or anything that
// is not a user JID — a group JID used as a history-sync fallback sender)
// leaves the namespace unknown, which is stored as NULL. This mirrors
// bareSenderUser, which normalises the same two forms in the other direction.
//
// The column holds the two namespaces a reader has to tell apart, so the
// hosted variants of each (business accounts) are recorded as the namespace
// they belong to rather than as a third and fourth value.
func splitSenderJID(sender string) (user string, server any) {
	user, srv, ok := strings.Cut(sender, "@")
	if !ok {
		return sender, nil
	}
	switch srv {
	case types.DefaultUserServer, types.HostedServer:
		return user, types.DefaultUserServer
	case types.HiddenUserServer, types.HostedLIDServer:
		return user, types.HiddenUserServer
	default:
		return user, nil
	}
}

// storedSender renders a resolved sender JID for the write paths: the bare
// user part and the namespace it belongs to, and nothing else. JID.String()
// is not used because it would spell an AD JID with its device suffix and a
// user-less JID as its bare server, neither of which belongs in
// messages.sender.
func storedSender(j types.JID) string {
	if j.User == "" {
		return ""
	}
	return j.User + "@" + j.Server
}

// legacySenderDigits are the bare-sender lengths that no longer tell us
// anything: the MCP server already reads more than 15 digits as a LID, and no
// phone number in a real archive is longer than 13, so a 14- or 15-digit
// sender that neither the LID map, nor the contact list, nor any chat JID
// knows is a LID whose mapping we never learned.
var legacySenderDigits = []int{14, 15}

// senderNamespaceClass is one backfill rule: every legacy row whose sender
// matches `where` belongs to `server`. The rules run in order and each only
// looks at rows still unclassified, so the evidence-based ones win over the
// length rule at the end.
type senderNamespaceClass struct {
	name   string
	server string
	where  string
}

// MigrateSenderNamespaces fills messages.sender_server for rows stored before
// the column existed, using whatsmeow's LID map and contact list plus our own
// chat JIDs as evidence. whatsapp.db is optional: without it (a restored
// backup, a store waiting to be re-paired) the rules that only need our own
// tables still run.
//
// It is safe on every start. Some senders can never be classified — a
// history-sync row attributed to a group JID, a number no side of the archive
// knows — so the pass does not retire; it costs one index range over the rows
// that are still unclassified, which is what the partial index on them holds.
// That is also what lets a row classify later, once whatsmeow has learned the
// mapping it was missing.
func (store *MessageStore) MigrateSenderNamespaces(whatsappDBPath string, logger waLog.Logger) error {
	pending, err := store.hasUnclassifiedSenders()
	if err != nil {
		return fmt.Errorf("failed to probe unclassified senders: %w", err)
	}
	if !pending {
		return nil
	}

	whatsappDB := true
	if _, err := os.Stat(whatsappDBPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat WhatsApp DB %s: %w", whatsappDBPath, err)
		}
		whatsappDB = false
		logger.Infof("Sender namespace backfill: %s not found, classifying from messages.db alone", whatsappDBPath)
	}

	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start sender namespace transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var alias string
	var hasLIDMap, hasContacts bool
	if whatsappDB {
		alias = fmt.Sprintf("wa_sender_ns_%d", time.Now().UnixNano())
		escapedPath := strings.ReplaceAll(whatsappDBPath, "'", "''")
		if _, err := tx.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS %s;", escapedPath, alias)); err != nil {
			return fmt.Errorf("failed to attach WhatsApp DB for sender namespace backfill: %w", err)
		}
		if hasLIDMap, err = attachedTableExists(tx, alias, "whatsmeow_lid_map"); err != nil {
			return err
		}
		if hasContacts, err = attachedTableExists(tx, alias, "whatsmeow_contacts"); err != nil {
			return err
		}
	}

	classes := senderNamespaceClasses(alias, hasLIDMap, hasContacts)
	counts := make([]string, 0, len(classes))
	var classified int64
	for _, class := range classes {
		// The class SQL is built from constants and the attach alias above,
		// never from user input.
		result, err := tx.Exec(fmt.Sprintf(
			`UPDATE messages SET sender_server = '%s'
			 WHERE sender_server IS NULL AND sender IS NOT NULL AND sender != '' AND (%s)`,
			class.server, class.where,
		))
		if err != nil {
			return fmt.Errorf("failed to backfill %s senders: %w", class.name, err)
		}
		updated, _ := result.RowsAffected()
		classified += updated
		counts = append(counts, fmt.Sprintf("%s=%d", class.name, updated))
	}

	var remaining int64
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE sender_server IS NULL",
	).Scan(&remaining); err != nil {
		return fmt.Errorf("failed to count unclassified senders: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit sender namespace backfill: %w", err)
	}

	// A store whose leftovers are all unclassifiable says so once per start at
	// DEBUG rather than repeating a line of zeroes at INFO forever.
	line := fmt.Sprintf("Sender namespace backfill: %s unclassified=%d", strings.Join(counts, " "), remaining)
	if classified > 0 {
		logger.Infof("%s", line)
	} else {
		logger.Debugf("%s", line)
	}
	return nil
}

// senderNamespaceClasses builds the backfill rules that the attached
// whatsmeow database can actually support. whatsmeow_lid_map is the only
// authoritative source for either namespace; whatsmeow_contacts and our own
// chats table are keyed by full JIDs, so the suffix says the namespace.
func senderNamespaceClasses(alias string, hasLIDMap, hasContacts bool) []senderNamespaceClass {
	classes := make([]senderNamespaceClass, 0, 4)
	if hasLIDMap {
		classes = append(classes,
			senderNamespaceClass{
				name:   "lid_map",
				server: types.HiddenUserServer,
				where:  fmt.Sprintf("sender IN (SELECT lid FROM %s.whatsmeow_lid_map WHERE lid != '')", alias),
			},
			senderNamespaceClass{
				name:   "phone_map",
				server: types.DefaultUserServer,
				where:  fmt.Sprintf("sender IN (SELECT pn FROM %s.whatsmeow_lid_map WHERE pn != '')", alias),
			},
		)
	}
	// A chat or contact row spells the namespace in its JID. LID chats do
	// survive here: resolveLIDChat leaves a chat it cannot map under its
	// @lid JID, and the chat migration only rewrites the mappable ones.
	classes = append(classes,
		senderNamespaceClass{
			name:   "lid_known",
			server: types.HiddenUserServer,
			where:  knownJIDEvidence(alias, hasContacts, "@lid"),
		},
		senderNamespaceClass{
			name:   "phone_known",
			server: types.DefaultUserServer,
			where:  knownJIDEvidence(alias, hasContacts, "@s.whatsapp.net"),
		},
	)

	globs := make([]string, 0, len(legacySenderDigits))
	for _, digits := range legacySenderDigits {
		globs = append(globs, fmt.Sprintf("sender GLOB '%s'", strings.Repeat("[0-9]", digits)))
	}
	classes = append(classes, senderNamespaceClass{
		name:   "lid_by_length",
		server: types.HiddenUserServer,
		where:  strings.Join(globs, " OR "),
	})
	return classes
}

// knownJIDEvidence matches senders that appear as a user JID of one namespace
// in a table we can read: our own chats, and whatsmeow's contact list when the
// database is attached.
func knownJIDEvidence(alias string, hasContacts bool, suffix string) string {
	terms := []string{fmt.Sprintf("EXISTS (SELECT 1 FROM chats c WHERE c.jid = messages.sender || '%s')", suffix)}
	if hasContacts {
		terms = append(terms, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM %s.whatsmeow_contacts k WHERE k.their_jid = messages.sender || '%s')",
			alias, suffix))
	}
	return strings.Join(terms, " OR ")
}

// hasUnclassifiedSenders reports whether any row still carries an unknown
// namespace. idx_messages_sender_server_null (store.go) makes this a lookup
// in an index that holds exactly those rows.
func (store *MessageStore) hasUnclassifiedSenders() (bool, error) {
	var hit int
	err := store.db.QueryRow(
		"SELECT 1 FROM messages WHERE sender_server IS NULL LIMIT 1",
	).Scan(&hit)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// attachedTableExists reports whether the attached whatsmeow database has a
// table; older whatsmeow versions predate whatsmeow_lid_map, and a store that
// never paired has neither table.
func attachedTableExists(tx *sql.Tx, alias, table string) (bool, error) {
	var count int
	if err := tx.QueryRow(fmt.Sprintf(
		"SELECT COUNT(1) FROM %s.sqlite_master WHERE type='table' AND name=?;", alias,
	), table).Scan(&count); err != nil {
		return false, fmt.Errorf("failed to inspect WhatsApp DB schema for %s: %w", table, err)
	}
	return count > 0, nil
}
