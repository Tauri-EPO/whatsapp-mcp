package main

// Native WhatsApp polls.
//
// A poll is two message kinds: PollCreationMessage (question + options,
// several proto versions) and PollUpdateMessage (a vote, encrypted with the
// poll's key; whatsmeow keeps the key in its message-secret store when it
// sees the creation and DecryptPollVote turns the update back into option
// hashes). Options are identified by SHA-256 of their text, so the bridge
// keeps the option list per poll to map votes back to names.
//
// Storage:
//   - messages: the creation is a normal row (media_type "poll", content
//     "📊 <question> — options: a | b | c") so it shows up in list_messages
//     and search; each vote is a row with media_type "poll_vote", content
//     "🗳️ <options>" and filename = the poll's message ID (same convention
//     as reactions).
//   - polls / poll_votes: structured copies used by /api/poll to tally
//     results. One row per voter per poll (latest vote wins).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const pollsSchema = `
	CREATE TABLE IF NOT EXISTS polls (
		message_id TEXT,
		chat_jid TEXT,
		question TEXT,
		options_json TEXT,
		selectable_count INTEGER,
		created_at TIMESTAMP,
		PRIMARY KEY (message_id, chat_jid)
	);
	CREATE TABLE IF NOT EXISTS poll_votes (
		poll_message_id TEXT,
		chat_jid TEXT,
		voter TEXT,
		selected_json TEXT,
		voted_at TIMESTAMP,
		PRIMARY KEY (poll_message_id, chat_jid, voter)
	);
`

// pollCreation is the version-independent view of a poll creation message.
type pollCreation struct {
	Question        string
	Options         []string
	SelectableCount int
}

// extractPollCreation returns the poll if msg carries one (any proto version).
func extractPollCreation(msg *waProto.Message) *pollCreation {
	if msg == nil {
		return nil
	}
	var pc *waProto.PollCreationMessage
	for _, candidate := range []*waProto.PollCreationMessage{
		msg.GetPollCreationMessage(), msg.GetPollCreationMessageV2(), msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(), msg.GetPollCreationMessageV6(),
	} {
		if candidate != nil {
			pc = candidate
			break
		}
	}
	if pc == nil {
		return nil
	}
	p := &pollCreation{Question: strings.TrimSpace(pc.GetName()), SelectableCount: int(pc.GetSelectableOptionsCount())}
	for _, opt := range pc.GetOptions() {
		if name := strings.TrimSpace(opt.GetOptionName()); name != "" {
			p.Options = append(p.Options, name)
		}
	}
	return p
}

// pollContent renders the creation as message text.
func pollContent(p *pollCreation) string {
	return fmt.Sprintf("📊 %s — options: %s", p.Question, strings.Join(p.Options, " | "))
}

func (store *MessageStore) StorePoll(messageID, chatJID string, p *pollCreation, createdAt time.Time) error {
	opts, err := json.Marshal(p.Options)
	if err != nil {
		return err
	}
	_, err = store.db.Exec(`INSERT INTO polls (message_id, chat_jid, question, options_json, selectable_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id, chat_jid) DO UPDATE SET question = excluded.question, options_json = excluded.options_json,
			selectable_count = excluded.selectable_count`,
		messageID, chatJID, p.Question, string(opts), p.SelectableCount, createdAt)
	return err
}

func (store *MessageStore) GetPoll(messageID, chatJID string) (*pollCreation, error) {
	var question, optsJSON string
	var selectable int
	err := store.db.QueryRow(`SELECT question, options_json, selectable_count FROM polls WHERE message_id = ? AND chat_jid = ?`,
		messageID, chatJID).Scan(&question, &optsJSON, &selectable)
	if err != nil {
		return nil, err
	}
	p := &pollCreation{Question: question, SelectableCount: selectable}
	if err := json.Unmarshal([]byte(optsJSON), &p.Options); err != nil {
		return nil, err
	}
	return p, nil
}

func (store *MessageStore) StorePollVote(pollMessageID, chatJID, voter string, selected []string, votedAt time.Time) error {
	sel, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	_, err = store.db.Exec(`INSERT INTO poll_votes (poll_message_id, chat_jid, voter, selected_json, voted_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(poll_message_id, chat_jid, voter) DO UPDATE SET selected_json = excluded.selected_json, voted_at = excluded.voted_at
		WHERE excluded.voted_at >= poll_votes.voted_at`,
		pollMessageID, chatJID, voter, string(sel), votedAt)
	return err
}

// optionNamesForHashes maps the SHA-256 option hashes of a vote back to names.
// Unknown hashes are reported as "?<hex-prefix>" rather than dropped so a
// stale option list is visible instead of silently losing votes.
func optionNamesForHashes(options []string, hashes [][]byte) []string {
	byHash := make(map[string]string, len(options))
	for _, name := range options {
		sum := sha256.Sum256([]byte(name))
		byHash[hex.EncodeToString(sum[:])] = name
	}
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		key := hex.EncodeToString(h)
		if name, ok := byHash[key]; ok {
			out = append(out, name)
		} else if len(key) >= 8 {
			out = append(out, "?"+key[:8])
		}
	}
	return out
}

// pollVoteDecrypter abstracts whatsmeow's DecryptPollVote for tests.
type pollVoteDecrypter func(ctx context.Context, evt *events.Message) ([][]byte, error)

func whatsmeowPollVoteDecrypter(client *whatsmeow.Client) pollVoteDecrypter {
	return func(ctx context.Context, evt *events.Message) ([][]byte, error) {
		vote, err := client.DecryptPollVote(ctx, evt)
		if err != nil {
			return nil, err
		}
		return vote.GetSelectedOptions(), nil
	}
}

// handlePollVote processes a PollUpdateMessage event: decrypts it, maps the
// selection to option names, stores the structured vote and a message row.
// Returns (handled, content) — handled=false means msg is not a poll vote.
func handlePollVote(ctx context.Context, decrypt pollVoteDecrypter, store *MessageStore, evt *events.Message, chatJID, sender string, ts time.Time, logger waLog.Logger) (handled bool, pollID string, content string) {
	update := evt.Message.GetPollUpdateMessage()
	if update == nil {
		return false, "", ""
	}
	pollID = update.GetPollCreationMessageKey().GetID()
	if pollID == "" {
		return true, "", ""
	}
	if decrypt == nil {
		logger.Warnf("Poll vote for %s in %s ignored: no decrypter configured", pollID, chatJID)
		return true, pollID, ""
	}
	hashes, err := decrypt(ctx, evt)
	if err != nil {
		logger.Warnf("Could not decrypt poll vote for %s in %s: %v", pollID, chatJID, err)
		return true, pollID, ""
	}
	var names []string
	poll, err := store.GetPoll(pollID, chatJID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logger.Warnf("Poll lookup failed for %s: %v", pollID, err)
		}
		names = optionNamesForHashes(nil, hashes)
	} else {
		names = optionNamesForHashes(poll.Options, hashes)
	}
	if err := store.StorePollVote(pollID, chatJID, sender, names, ts); err != nil {
		logger.Warnf("Failed to store poll vote: %v", err)
	}
	if len(names) == 0 {
		return true, pollID, "🗳️ vote retracted"
	}
	return true, pollID, "🗳️ voted: " + strings.Join(names, ", ")
}

// --- /api/poll --------------------------------------------------------------

type PollResultsResponse struct {
	Success         bool              `json:"success"`
	Message         string            `json:"message,omitempty"`
	MessageID       string            `json:"message_id,omitempty"`
	ChatJID         string            `json:"chat_jid,omitempty"`
	Question        string            `json:"question,omitempty"`
	SelectableCount int               `json:"selectable_count,omitempty"`
	TotalVoters     int               `json:"total_voters"`
	Options         []PollOptionTally `json:"options"`
	Votes           []PollVoteEntry   `json:"votes"`
}

type PollOptionTally struct {
	Name   string   `json:"name"`
	Count  int      `json:"count"`
	Voters []string `json:"voters"`
}

type PollVoteEntry struct {
	Voter    string    `json:"voter"`
	Selected []string  `json:"selected"`
	VotedAt  time.Time `json:"voted_at"`
}

func (store *MessageStore) PollResults(messageID, chatJID string) (*PollResultsResponse, error) {
	poll, err := store.GetPoll(messageID, chatJID)
	if err != nil {
		return nil, err
	}
	resp := &PollResultsResponse{Success: true, MessageID: messageID, ChatJID: chatJID, Question: poll.Question,
		SelectableCount: poll.SelectableCount, Options: make([]PollOptionTally, 0, len(poll.Options)), Votes: []PollVoteEntry{}}
	tally := map[string]*PollOptionTally{}
	for _, name := range poll.Options {
		resp.Options = append(resp.Options, PollOptionTally{Name: name, Voters: []string{}})
		tally[name] = &resp.Options[len(resp.Options)-1]
	}
	rows, err := store.db.Query(`SELECT voter, selected_json, voted_at FROM poll_votes WHERE poll_message_id = ? AND chat_jid = ? ORDER BY voted_at`, messageID, chatJID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var entry PollVoteEntry
		var selJSON string
		if err := rows.Scan(&entry.Voter, &selJSON, &entry.VotedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(selJSON), &entry.Selected)
		if entry.Selected == nil {
			entry.Selected = []string{}
		}
		resp.Votes = append(resp.Votes, entry)
		if len(entry.Selected) > 0 {
			resp.TotalVoters++
		}
		for _, name := range entry.Selected {
			if t, ok := tally[name]; ok {
				t.Count++
				t.Voters = append(t.Voters, entry.Voter)
			}
		}
	}
	return resp, rows.Err()
}

// handlePollResults serves GET /api/poll?message_id=...&chat_jid=...
func handlePollResults(store *MessageStore, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		messageID := strings.TrimSpace(r.URL.Query().Get("message_id"))
		chatJID := strings.TrimSpace(r.URL.Query().Get("chat_jid"))
		if messageID == "" || chatJID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(PollResultsResponse{Message: "message_id and chat_jid are required", Options: []PollOptionTally{}, Votes: []PollVoteEntry{}})
			return
		}
		if _, err := types.ParseJID(chatJID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(PollResultsResponse{Message: "invalid chat_jid", Options: []PollOptionTally{}, Votes: []PollVoteEntry{}})
			return
		}
		if rejectByChatPolicy(w, policy, chatJID) {
			return
		}
		resp, err := store.PollResults(messageID, chatJID)
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(PollResultsResponse{Message: "no poll with that message_id in this chat", Options: []PollOptionTally{}, Votes: []PollVoteEntry{}})
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(PollResultsResponse{Message: err.Error(), Options: []PollOptionTally{}, Votes: []PollVoteEntry{}})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
