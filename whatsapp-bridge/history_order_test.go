package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestHistorySessionReadStallDoesNotHoldArchiveWriter(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, cancelHistory := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "cancel"}[cancelHistory], func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
			if err != nil {
				t.Fatal(err)
			}
			boundPool(session, sessionPoolConns)
			t.Cleanup(func() { _ = session.Close() })
			if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
				t.Fatal(err)
			}
			container := sqlstore.NewWithDB(session, "sqlite", testLogger())
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(container.LIDMap), ms, rec)
			b.MediaAutoDownload = false
			// Exhaust the actual bounded session pool. The SDK read will park
			// in database/sql until a connection returns or bridge ctx cancels.
			var held []*sql.Conn
			for range sessionPoolConns {
				conn, err := session.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, conn)
				t.Cleanup(func() { _ = conn.Close() })
			}
			fixture := shareHistoryFixture(1)
			fixture.Data.Conversations[0].Messages[0].Message.Participant = proto.String(phoneLID.String())
			fixture.Data.Conversations = append(fixture.Data.Conversations, largeHistoryFixture(1).Data.Conversations[0])
			done := make(chan struct{})
			go func() { defer close(done); b.handleHistorySync(fixture) }()
			deadline := time.After(2 * time.Second)
			for session.Stats().WaitCount == 0 {
				select {
				case <-deadline:
					t.Fatal("SDK read never reached occupied session pool")
				default:
					runtime.Gosched()
				}
			}
			// A real live insertion must finish while the SDK remains blocked.
			liveDone := make(chan struct{})
			go func() {
				defer close(liveDone)
				event := buildTextMessage(selfPhone, selfPhone, types.EmptyJID, types.EmptyJID, false, "liveword")
				event.Info.ID = "LIVE-STALL"
				b.handleEvent(event, nil)
			}()
			select {
			case <-liveDone:
			case <-time.After(2 * time.Second):
				t.Fatal("history session read held archive writer")
			}
			if b.metrics.messagesStored.Load() != 1 || b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("live write lost: stored=%d failures=%d", b.metrics.messagesStored.Load(), b.metrics.storeFailures.Load())
			}
			if cancelHistory {
				b.cancel()
			} else {
				for _, conn := range held {
					_ = conn.Close()
				}
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("history did not unblock on release/cancellation")
			}
			var historyRows, indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='H0'").Scan(&historyRows); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			want := 2
			if cancelHistory {
				want = 0
			}
			if historyRows != want || indexed != want || len(errorLines(rec.String())) != 0 {
				t.Fatalf("history=%d FTS=%d log=%s", historyRows, indexed, rec.String())
			}
		})
	}
}

func TestHistoryIdentityPreparationFailureAffectsOnlyItsRow(t *testing.T) {
	for _, mode := range []string{"missing-session-table", "restored-session-schema", "released-session-writer"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			session, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "session.db"))+"?_pragma=busy_timeout(1)")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			session.SetMaxOpenConns(2)
			var held *sql.Conn
			if mode == "released-session-writer" {
				if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
					t.Fatal(err)
				}
				held, err = session.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = held.ExecContext(context.Background(), "ROLLBACK")
					_ = held.Close()
				})
				if _, err := held.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
					t.Fatal(err)
				}
			}
			container := sqlstore.NewWithDB(session, "sqlite", testLogger())
			b := testBridge(t, newTestClientWithSelf(container.LIDMap, selfPhone), ms, testLogger())
			if mode == "restored-session-schema" {
				restored := false
				b.historyBatchWriter = func(write func(*messageBatch) error) error {
					err := ms.Batch(write)
					if err != nil && !restored {
						// Restore after the failed archive batch has rolled back.
						_, schemaErr := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User)
						if schemaErr != nil {
							t.Fatal(schemaErr)
						}
						restored = true
					}
					return err
				}
			}
			stamp := time.Unix(1772359200, 0)
			if err := ms.StoreChat(phonePN.String(), "Alice", stamp); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage(storedMessage{ID: "H2", ChatJID: phonePN.String(), Sender: phoneLID.String(), Content: "initialsample", Timestamp: stamp, IsFromMe: false, MediaType: "location", Filename: "", URL: "", MediaKey: nil, FileSHA256: nil, FileEncSHA256: nil, FileLength: 0, QuotedMessageID: ""}); err != nil {
				t.Fatal(err)
			}
			waits := 0
			b.storeRetryWait = func(time.Duration) bool {
				waits++
				// The SDK writer is still locked: a real archive write must be free.
				if err := ms.StoreChat(selfPhone.String(), "Bob", stamp); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "LIVE-PREP", ChatJID: selfPhone.String(), Sender: selfPhone.String(), Content: "prepwriterword", Timestamp: stamp, IsFromMe: false, MediaType: "", Filename: "", URL: "", MediaKey: nil, FileSHA256: nil, FileEncSHA256: nil, FileLength: 0, QuotedMessageID: ""}); err != nil {
					t.Fatal(err)
				}
				if held == nil {
					t.Fatal("non-busy preparation should not wait")
				}
				if _, err := held.ExecContext(context.Background(), "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
				return true
			}
			fixture := largeHistoryFixture(historyBatchMessages + 2)
			fixture.Data.Conversations[0].Messages[2].Message.Message = livePosition(2, 0.25)
			b.handleHistorySync(fixture)
			var rows, indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			wantRows, wantFailures, wantWaits := historyBatchMessages+2, int64(1), 0
			if held != nil {
				wantRows, wantFailures, wantWaits = wantRows+1, 0, 1
			}
			if mode == "restored-session-schema" {
				wantFailures = 0
			}
			if rows != wantRows || indexed != historyBatchMessages+1 || b.metrics.historyMessages.Load() != int64(historyBatchMessages+1) || b.metrics.storeFailures.Load() != wantFailures || waits != wantWaits {
				t.Fatalf("rows=%d FTS=%d committed=%d failures=%d waits=%d", rows, indexed, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load(), waits)
			}
		})
	}
}

func TestHistoryConversationVoteOrderingAndReplay(t *testing.T) {
	for iteration := range 12 {
		ms, _ := lockedProductionStore(t)
		b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
		b.PollVoteDecrypt = func(ctx context.Context, evt *events.Message) ([][]byte, error) {
			for range iteration + 1 {
				runtime.Gosched()
			}
			return [][]byte{hashOf("Sushi")}, ctx.Err()
		}
		stamp := time.Unix(1772359200, 0)
		fixture := historySyncWithPoll(phonePN, stamp)
		other := historySyncWithPoll(selfPhone, stamp)
		fixture.Data.Conversations = append(fixture.Data.Conversations, other.Data.Conversations...)
		for range 2 { // idempotent replay preserves rowid order and tally
			b.handleHistorySync(fixture)
			b.historyVotes.Wait()
			rows, err := ms.db.Query("SELECT id || ':' || chat_jid FROM messages ORDER BY rowid")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					t.Fatal(err)
				}
				got = append(got, value)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close() // drain/close before any subsequent database work
			want := []string{"HPOLL1:" + phonePN.String(), "HPOLL1:" + selfPhone.String(), "HVOTE1:" + phonePN.String(), "HVOTE1:" + selfPhone.String()}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("iteration=%d ordering=%v", iteration, got)
			}
			for _, chat := range []types.JID{phonePN, selfPhone} {
				result, err := ms.PollResults("HPOLL1", chat.String())
				if err != nil || result.TotalVoters != 1 || result.Options[1].Count != 1 {
					t.Fatalf("tally=%+v err=%v", result, err)
				}
			}
			var indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Sushi'").Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			if indexed != 4 || b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("FTS=%d failures=%d", indexed, b.metrics.storeFailures.Load())
			}
		}
	}
}

func TestHistoryMissingVoteSecretsSharePayloadRetryBudget(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	b.HistoryVoteRetryDelays = []time.Duration{time.Second, time.Second}
	chats := []types.JID{phonePN, selfPhone, phoneLID, selfLID}
	var mu sync.Mutex
	calls := map[string]int{}
	firstPass, ready := make(chan struct{}), make(chan struct{})
	b.PollVoteDecrypt = func(_ context.Context, evt *events.Message) ([][]byte, error) {
		if evt.Info.ID == "READY" {
			close(ready)
			return [][]byte{hashOf("Sushi")}, nil
		}
		mu.Lock()
		defer mu.Unlock()
		calls[evt.Info.Chat.String()]++
		if calls[evt.Info.Chat.String()] == 1 && len(calls) == len(chats) {
			close(firstPass)
		}
		if calls[evt.Info.Chat.String()] > 1 && len(calls) != len(chats) {
			t.Errorf("retry started before all initial votes: calls=%v", calls)
		}
		return nil, whatsmeow.ErrOriginalMessageSecretNotFound
	}
	stamp := time.Unix(1772359200, 0)
	fixture := historySyncWithPoll(chats[0], stamp)
	for _, chat := range chats[1:] {
		fixture.Data.Conversations = append(fixture.Data.Conversations, historySyncWithPoll(chat, stamp).Data.Conversations...)
	}
	started := time.Now()
	b.handleHistorySync(fixture)
	select {
	case <-firstPass:
	case <-time.After(10 * time.Second):
		t.Fatal("initial vote pass never completed")
	}
	later := historySyncWithPoll(phonePN, stamp.Add(time.Hour))
	later.Data.Conversations[0].Messages[0].Message.Key.ID = proto.String("READY")
	// The poll already exists; this payload carries only the later vote.
	later.Data.Conversations[0].Messages = later.Data.Conversations[0].Messages[:1]
	b.handleHistorySync(later)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Error("later ready vote waited behind missing-secret retry sleeps")
	}
	b.historyVotes.Wait()
	elapsed := time.Since(started)
	// One two-second budget has a generous scheduling/SQLite allowance;
	// the old four independent budgets require at least eight seconds.
	if elapsed >= 6*time.Second {
		t.Errorf("four conversations multiplied the two-second retry budget: %s", elapsed)
	}
	t.Logf("four missing-secret conversations plus a later ready vote: %s", elapsed)
	for _, chat := range chats {
		result, err := ms.PollResults("HPOLL1", chat.String())
		wantMissing, wantReady := 1, 0
		if chat == phonePN {
			wantMissing, wantReady = 0, 1
		}
		if err != nil || result.UndecodableVotes != wantMissing || result.TotalVoters != wantReady {
			t.Fatalf("chat=%s tally=%+v err=%v", chat, result, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, chat := range chats {
		if calls[chat.String()] != 3 {
			t.Errorf("chat=%s attempts=%d, want initial plus two retry passes", chat, calls[chat.String()])
		}
	}
}

func TestHistoryPendingVoteCannotOverwriteLaterEqualTimeVote(t *testing.T) {
	for _, mode := range []string{"undecodable", "decoded", "older-ready", "live-ready", "live-during-initial"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.HistoryVoteRetryDelays = []time.Duration{time.Millisecond}
			missing, ready := make(chan struct{}), make(chan struct{})
			calls := 0
			b.PollVoteDecrypt = func(ctx context.Context, evt *events.Message) ([][]byte, error) {
				if evt.Info.ID == "READY" {
					close(ready)
					return [][]byte{hashOf("Sushi")}, nil
				}
				calls++
				if calls == 1 {
					close(missing)
					if mode != "live-during-initial" {
						return nil, whatsmeow.ErrOriginalMessageSecretNotFound
					}
				}
				select {
				case <-ready:
					deadline := time.NewTimer(2 * time.Second)
					defer deadline.Stop()
					// Observe the actual newer archive row before releasing the old
					// decoder; a callback notification alone is not commit proof.
					for {
						var committed int
						if err := ms.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE id='READY'").Scan(&committed); err != nil {
							return nil, err
						}
						if committed == 1 {
							break
						}
						select {
						case <-time.After(time.Millisecond):
						case <-deadline.C:
							return nil, context.DeadlineExceeded
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					if mode == "decoded" {
						return [][]byte{hashOf("Pizza")}, nil
					}
					return nil, whatsmeow.ErrOriginalMessageSecretNotFound
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			stamp := time.Unix(1772359200, 0)
			b.handleHistorySync(historySyncWithPoll(phonePN, stamp))
			select {
			case <-missing:
			case <-time.After(2 * time.Second):
				t.Fatal("initial missing vote was never decoded")
			}
			readyStamp := stamp
			if mode == "older-ready" {
				readyStamp = stamp.Add(-time.Second)
			}
			later := historySyncWithPoll(phonePN, readyStamp)
			later.Data.Conversations[0].Messages[0].Message.Key.ID = proto.String("READY")
			later.Data.Conversations[0].Messages = later.Data.Conversations[0].Messages[:1]
			if mode == "live-ready" || mode == "live-during-initial" {
				event, err := b.currentClient().ParseWebMessage(phonePN, later.Data.Conversations[0].Messages[0].Message)
				if err != nil {
					t.Fatal(err)
				}
				b.handleEvent(event, nil)
			} else {
				b.handleHistorySync(later)
			}
			b.historyVotes.Wait()
			result, err := ms.PollResults("HPOLL1", phonePN.String())
			wantReady, wantMissing := 1, 0
			if mode == "older-ready" {
				wantReady, wantMissing = 0, 1
			}
			if err != nil || result.TotalVoters != wantReady || result.UndecodableVotes != wantMissing || result.Options[1].Count != wantReady {
				t.Fatalf("mode=%s equal-time tally=%+v err=%v", mode, result, err)
			}
			var oldRows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='HVOTE1' AND content LIKE '%Pizza%'").Scan(&oldRows); err != nil {
				t.Fatal(err)
			}
			wantOld := 0
			if mode == "decoded" {
				wantOld = 1 // archive the old vote without reverting the latest tally
			}
			if oldRows != wantOld {
				t.Fatalf("older decoded vote archive rows=%d", oldRows)
			}
			b.historyVoteOrderMu.Lock()
			retained := len(b.historyPendingVotes)
			b.historyVoteOrderMu.Unlock()
			if retained != 0 {
				t.Fatalf("finished vote jobs retained %d ordering entries", retained)
			}
		})
	}
}

func TestHistoryVoteRetryShutdownStopsFollowingVoteJobs(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	entered := make(chan struct{})
	b.PollVoteDecrypt = func(ctx context.Context, _ *events.Message) ([][]byte, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	fixture := historySyncWithPoll(phonePN, time.Unix(1772359200, 0))
	fixture.Data.Conversations = append(fixture.Data.Conversations, historySyncWithPoll(selfPhone, time.Unix(1772359200, 0)).Data.Conversations...)
	b.handleHistorySync(fixture)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("vote decoder never entered")
	}
	// A later notification queues behind the blocked first vote batch.
	b.handleHistorySync(historySyncWithPoll(selfPhone, time.Unix(1772359200, 0)))
	b.Shutdown(2 * time.Second)
	var rows int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE media_type='poll_vote'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("following rows=%d failures=%d", rows, b.metrics.storeFailures.Load())
	}
}

func TestHistoryVoteSecretInNextSDKNotificationDoesNotBlockCallback(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	secret, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.PollVoteDecrypt = func(ctx context.Context, _ *events.Message) ([][]byte, error) {
		once.Do(func() { close(entered) })
		select {
		case <-secret:
			return [][]byte{hashOf("Sushi")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	firstReturned, secondReturned := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(secondReturned)
		// The pinned SDK runs callbacks in its notification loop. Only after
		// the first callback returns can it load the next payload's secret.
		b.handleEvent(historySyncWithPoll(phonePN, time.Unix(1772359200, 0)), nil)
		close(firstReturned)
		select {
		case <-entered:
		case <-b.ctx.Done():
			return
		}
		close(secret)
		b.handleEvent(historySyncWithPoll(selfPhone, time.Unix(1772359200, 0)), nil)
	}()
	select {
	case <-firstReturned:
	case <-time.After(2 * time.Second):
		b.cancel()
		t.Fatal("SDK callback blocked the payload containing its vote secret")
	}
	select {
	case <-secondReturned:
	case <-time.After(2 * time.Second):
		b.cancel()
		t.Fatal("next SDK notification could not load its secret")
	}
	b.historyVotes.Wait()
	rows, err := ms.db.Query("SELECT chat_jid FROM messages WHERE media_type='poll_vote' ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var chat string
		if err := rows.Scan(&chat); err != nil {
			t.Fatal(err)
		}
		got = append(got, chat)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if !reflect.DeepEqual(got, []string{phonePN.String(), selfPhone.String()}) {
		t.Fatalf("FIFO vote jobs interleaved: %v", got)
	}
	for _, chat := range got {
		result, err := ms.PollResults("HPOLL1", chat)
		if err != nil || result.TotalVoters != 1 || result.UndecodableVotes != 0 {
			t.Fatalf("later payload secret lost: %+v err=%v", result, err)
		}
	}
	if b.metrics.storeFailures.Load() != 0 {
		t.Fatal("successful secret import reported archive failures")
	}
}

func TestHistoryVoteDeliveredByRetiredRuntimeCannotWriteAfterHandoff(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	first := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
	second := newTestClientWithSelf(&mockLIDStore{}, phonePN)
	b := testBridge(t, first, ms, testLogger())
	reconnect := make(chan bool, 1)
	b.installClient(first, true, reconnect)
	entered, release := make(chan struct{}), make(chan struct{})
	b.PollVoteDecrypt = func(ctx context.Context, _ *events.Message) ([][]byte, error) {
		close(entered)
		select {
		case <-release:
			return [][]byte{hashOf("Sushi")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b.handleClientEvent(first, historySyncWithPoll(phonePN, time.Unix(1772359200, 0)), reconnect)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("history vote never entered decryption")
	}
	// Handoff must finish while decryption is blocked; only the archive write
	// briefly holds the read gate and rechecks the delivery runtime.
	handoff := make(chan struct{})
	go func() { b.installClient(second, true, reconnect); close(handoff) }()
	select {
	case <-handoff:
	case <-time.After(2 * time.Second):
		b.cancel()
		close(release)
		t.Fatal("decryption held the runtime handoff gate")
	}
	close(release)
	b.historyVotes.Wait()
	var votes, rows int
	if err := ms.db.QueryRow("SELECT count(*) FROM poll_votes").Scan(&votes); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT count(*) FROM messages WHERE media_type='poll_vote'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if votes != 0 || rows != 0 || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("retired payload persisted after handoff: votes=%d rows=%d failures=%d", votes, rows, b.metrics.storeFailures.Load())
	}
}
