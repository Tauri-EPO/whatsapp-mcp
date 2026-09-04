package main

import (
	"context"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// resolveQuotedParticipantJID normalises the quoted_sender_jid an API caller
// passes for a reply into the form recipient clients can match against a chat
// member. messages.db stores senders as bare user-parts ("15551234567"), and
// callers often echo that back; a ContextInfo.Participant with no server (or a
// phone JID in a LID-addressed group) matches nobody, so the quoted bubble
// renders as "You" for every viewer and the quote notification misfires.
//
//   - ""                      -> "" (nothing to attribute)
//   - "123@s.whatsapp.net"    -> LID from the store if known, else unchanged
//   - "123"                   -> "123@s.whatsapp.net", upgraded to LID if known
//   - "abc@lid" / other JIDs  -> unchanged
//   - unparseable             -> unchanged (let WhatsApp reject it, don't guess)
//
// Same LID upgrade rule as resolveMentionJIDs; see AGENTS.md gotcha #1.
func resolveQuotedParticipantJID(client *whatsmeow.Client, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var jid types.JID
	if strings.Contains(raw, "@") {
		parsed, err := types.ParseJID(raw)
		if err != nil {
			return raw
		}
		jid = parsed
	} else {
		jid = types.NewJID(raw, types.DefaultUserServer)
	}
	if jid.Server == types.DefaultUserServer && client != nil && client.Store != nil && client.Store.LIDs != nil {
		if lid, err := client.Store.LIDs.GetLIDForPN(context.Background(), jid.ToNonAD()); err == nil && !lid.IsEmpty() {
			return lid.String()
		}
	}
	return jid.String()
}
