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
//     results. One row per voter per poll (latest vote wins). A vote whose
//     payload could not be decrypted is kept with selected_json NULL so
//     /api/poll can report undecodable_votes instead of silently
//     under-counting (issue #59).
//
// Polls created before the bridge ran: whatsmeow persists message secrets
// delivered by history sync (storeHistoricalMessageSecrets), so votes for
// any poll the phone included in the sync decode normally. Votes cast
// while the bridge was down arrive in the same sync as PollUpdateMessage
// rows and are decoded after the conversation is stored (the poll row must
// exist first, and whatsmeow writes the secrets asynchronously, hence the
// short retry). Only polls the phone never synced stay undecodable.

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
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
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
func extractPollCreation(msg *waE2E.Message) *pollCreation {
	if msg == nil {
		return nil
	}
	var pc *waE2E.PollCreationMessage
	for _, candidate := range []*waE2E.PollCreationMessage{
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
	return store.upsertPollVote(pollMessageID, chatJID, voter, sql.NullString{String: string(sel), Valid: true}, votedAt)
}

// StoreUndecodablePollVote records that voter cast a vote we could not
// decrypt (selected_json NULL). A later decodable vote from the same voter
// replaces it.
func (store *MessageStore) StoreUndecodablePollVote(pollMessageID, chatJID, voter string, votedAt time.Time) error {
	return store.upsertPollVote(pollMessageID, chatJID, voter, sql.NullString{}, votedAt)
}

func (store *MessageStore) upsertPollVote(pollMessageID, chatJID, voter string, selected sql.NullString, votedAt time.Time) error {
	_, err := store.db.Exec(`INSERT INTO poll_votes (poll_message_id, chat_jid, voter, selected_json, voted_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(poll_message_id, chat_jid, voter) DO UPDATE SET selected_json = excluded.selected_json, voted_at = excluded.voted_at
		WHERE excluded.voted_at >= poll_votes.voted_at`,
		pollMessageID, chatJID, voter, selected, votedAt)
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

var errNoPollVoteDecrypter = errors.New("no poll vote decrypter configured")

// decodePollVote decrypts a PollUpdateMessage and maps the selection to option
// names. pollID is empty when evt is not a poll vote (or carries no poll key).
func decodePollVote(ctx context.Context, decrypt pollVoteDecrypter, store *MessageStore, evt *events.Message, chatJID string, logger waLog.Logger) (pollID string, names []string, err error) {
	update := evt.Message.GetPollUpdateMessage()
	if update == nil {
		return "", nil, nil
	}
	pollID = update.GetPollCreationMessageKey().GetID()
	if pollID == "" {
		return "", nil, nil
	}
	if decrypt == nil {
		return pollID, nil, errNoPollVoteDecrypter
	}
	hashes, err := decrypt(ctx, evt)
	if err != nil {
		return pollID, nil, err
	}
	poll, err := store.GetPoll(pollID, chatJID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logger.Warnf("Poll lookup failed for %s: %v", pollID, err)
		}
		return pollID, optionNamesForHashes(nil, hashes), nil
	}
	return pollID, optionNamesForHashes(poll.Options, hashes), nil
}

// pollVoteContent renders a decoded vote as message text.
func pollVoteContent(names []string) string {
	if len(names) == 0 {
		return "🗳️ vote retracted"
	}
	return "🗳️ voted: " + strings.Join(names, ", ")
}

// handlePollVote processes a live PollUpdateMessage event: decrypts it, maps
// the selection to option names and stores the structured vote. Returns
// (handled, pollID, content) — handled=false means evt is not a poll vote;
// an empty content with handled=true means the vote was recorded as
// undecodable and no message row should be written.
func handlePollVote(ctx context.Context, decrypt pollVoteDecrypter, store *MessageStore, evt *events.Message, chatJID, sender string, ts time.Time, logger waLog.Logger) (handled bool, pollID string, content string) {
	if evt.Message.GetPollUpdateMessage() == nil {
		return false, "", ""
	}
	pollID, names, err := decodePollVote(ctx, decrypt, store, evt, chatJID, logger)
	if pollID == "" {
		return true, "", ""
	}
	if err != nil {
		logger.Warnf("Could not decrypt poll vote for %s in %s: %v", pollID, chatJID, err)
		if serr := store.StoreUndecodablePollVote(pollID, chatJID, sender, ts); serr != nil {
			logger.Warnf("Failed to record undecodable poll vote: %v", serr)
		}
		return true, pollID, ""
	}
	if err := store.StorePollVote(pollID, chatJID, sender, names, ts); err != nil {
		logger.Warnf("Failed to store poll vote: %v", err)
	}
	return true, pollID, pollVoteContent(names)
}

// storePollVoteMessage writes the message row for a decoded vote (media_type
// poll_vote, target = the poll's message ID), shared by live and history paths.
func (store *MessageStore) storePollVoteMessage(id, chatJID, sender, content string, ts time.Time, fromMe bool, pollID string, logger waLog.Logger) {
	if err := store.StoreMessage(id, chatJID, sender, content, ts, fromMe, "poll_vote", pollID, "", nil, nil, nil, 0, ""); err != nil {
		logger.Warnf("Failed to store poll vote: %v", err)
		return
	}
	if err := store.SetTargetMessageID(id, chatJID, pollID); err != nil {
		logger.Warnf("Failed to set poll vote target: %v", err)
	}
}

// historyVoteRetryDelays paces retries while whatsmeow is still writing the
// secrets it received in the same history-sync chunk (it stores them
// asynchronously). Tests shorten it.
var historyVoteRetryDelays = []time.Duration{2 * time.Second, 10 * time.Second}

// storeHistoryPollVotes decodes PollUpdateMessage rows delivered by history
// sync for one conversation. Runs after the conversation's messages (and
// therefore the poll rows) are stored; each vote retries briefly when the
// poll secret is not there yet and is recorded as undecodable otherwise.
func (b *Bridge) storeHistoryPollVotes(chat types.JID, chatJID string, votes []*waWeb.WebMessageInfo, done chan<- struct{}) {
	defer func() {
		if done != nil {
			close(done)
		}
	}()
	for _, web := range votes {
		evt, err := b.Client.ParseWebMessage(chat, web)
		if err != nil {
			b.Log.Warnf("Could not parse history poll vote %s: %v", web.GetKey().GetID(), err)
			continue
		}
		sender := resolveUserJID(b.Client, evt.Info.Sender, types.EmptyJID).User
		for attempt := 0; ; attempt++ {
			pollID, names, derr := decodePollVote(context.Background(), b.PollVoteDecrypt, b.Store, evt, chatJID, b.Log)
			if pollID == "" {
				break
			}
			if derr == nil {
				if serr := b.Store.StorePollVote(pollID, chatJID, sender, names, evt.Info.Timestamp); serr != nil {
					b.Log.Warnf("Failed to store history poll vote: %v", serr)
				}
				b.Store.storePollVoteMessage(evt.Info.ID, chatJID, sender, pollVoteContent(names), evt.Info.Timestamp, evt.Info.IsFromMe, pollID, b.Log)
				break
			}
			if errors.Is(derr, whatsmeow.ErrOriginalMessageSecretNotFound) && attempt < len(historyVoteRetryDelays) {
				time.Sleep(historyVoteRetryDelays[attempt])
				continue
			}
			b.Log.Warnf("History poll vote %s for %s in %s is undecodable: %v", evt.Info.ID, pollID, chatJID, derr)
			if serr := b.Store.StoreUndecodablePollVote(pollID, chatJID, sender, evt.Info.Timestamp); serr != nil {
				b.Log.Warnf("Failed to record undecodable poll vote: %v", serr)
			}
			break
		}
	}
}

// --- /api/poll --------------------------------------------------------------

type PollResultsResponse struct {
	Success         bool   `json:"success"`
	Message         string `json:"message,omitempty"`
	MessageID       string `json:"message_id,omitempty"`
	ChatJID         string `json:"chat_jid,omitempty"`
	Question        string `json:"question,omitempty"`
	SelectableCount int    `json:"selectable_count,omitempty"`
	TotalVoters     int    `json:"total_voters"`
	// UndecodableVotes counts voters whose vote could not be decrypted (the
	// poll's secret was never seen by this bridge). They are excluded from
	// TotalVoters and the option tallies.
	UndecodableVotes int               `json:"undecodable_votes"`
	Options          []PollOptionTally `json:"options"`
	Votes            []PollVoteEntry   `json:"votes"`
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
		var selJSON sql.NullString
		if err := rows.Scan(&entry.Voter, &selJSON, &entry.VotedAt); err != nil {
			return nil, err
		}
		if !selJSON.Valid {
			resp.UndecodableVotes++
			continue
		}
		_ = json.Unmarshal([]byte(selJSON.String), &entry.Selected)
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
