package main

import (
	"context"
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
//   - malformed/empty        -> omitted with a warning
//
// Same LID upgrade rule as resolveMentionJIDs; see AGENTS.md gotcha #1.
func resolveQuotedParticipantJID(client *whatsmeow.Client, raw string) string {
	return resolveQuotedParticipantJIDContext(context.Background(), client, raw)
}

func resolveQuotedParticipantJIDContext(ctx context.Context, client *whatsmeow.Client, raw string) string {
	if raw == "" {
		return ""
	}
	jid, err := normalizedUserJID(raw)
	if err != nil {
		bridgeLog.Warnf("skipping unparseable quoted sender %q: %v", raw, err)
		return ""
	}
	if jid.Server == types.DefaultUserServer {
		if lid, err := lookupAltJID(ctx, client, jid.ToNonAD()); err == nil && !lid.IsEmpty() {
			return lid.String()
		}
	}
	return jid.String()
}
