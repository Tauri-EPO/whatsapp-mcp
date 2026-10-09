package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"time"
)

type exportStore struct {
	db *archiveDatabase
	tx *sql.Tx
}

func (s *exportStore) close() {
	if s.tx != nil {
		_ = s.tx.Rollback()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}
func openExportStore(ctx context.Context, root *os.Root, name string, optional bool) (exportStore, error) {
	db, err := openArchiveDB(root, name, optional)
	s := exportStore{db: db}
	if err != nil || db == nil {
		return s, err
	}
	s.tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err == nil {
		var n int
		err = s.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema").Scan(&n)
	}
	if err != nil {
		s.close()
	}
	return s, err
}

func exportRows(ctx context.Context, tx *sql.Tx, query string, out io.Writer, array bool, transform func(map[string]any) error) (int64, error) {
	var rows *sql.Rows
	if tx != nil {
		var err error
		rows, err = tx.QueryContext(ctx, query)
		if err != nil {
			return 0, err
		}
		defer func() { _ = rows.Close() }()
	}
	var count int64
	if array {
		if _, err := io.WriteString(out, "["); err != nil {
			return count, err
		}
	}
	if rows != nil {
		columns, err := rows.Columns()
		if err != nil {
			return count, err
		}
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return count, err
			}
			if err := rows.Scan(pointers...); err != nil {
				return count, err
			}
			row := make(map[string]any, len(columns))
			for i, name := range columns {
				row[name] = values[i]
			}
			if transform != nil {
				if err := transform(row); err != nil {
					return count, err
				}
			}
			if array && count > 0 {
				if _, err := io.WriteString(out, ","); err != nil {
					return count, err
				}
			}
			if err := json.NewEncoder(out).Encode(row); err != nil {
				return count, err
			}
			count++
		}
		if err := rows.Err(); err != nil {
			return count, err
		}
	}
	if array {
		if _, err := io.WriteString(out, "]\n"); err != nil {
			return count, err
		}
	}
	return count, nil
}

func exportTableExists(ctx context.Context, tx *sql.Tx, name string) bool {
	if tx == nil {
		return false
	}
	var count int
	return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=?", name).Scan(&count) == nil && count == 1
}

type exportCounts struct{ skippedMedia int64 }

func (b *Bridge) exportWalk(ctx context.Context, messages, notes, contacts *sql.Tx, media bool, counts ...*exportCounts) archiveWalk {
	return func(emit func(archiveEntry) error) error {
		if len(counts) > 0 {
			counts[0].skippedMedia = 0
		}
		for _, file := range []struct {
			name, query, table string
			tx                 *sql.Tx
			array              bool
		}{
			{"chats.json", "SELECT jid, name, CAST(last_message_time AS TEXT) AS last_message_time FROM chats ORDER BY jid", "chats", messages, true},
			{"contacts.json", "SELECT their_jid AS jid, first_name, full_name, push_name, business_name FROM whatsmeow_contacts ORDER BY our_jid, their_jid", "whatsmeow_contacts", contacts, true},
			{"messages.jsonl", "SELECT id, chat_jid AS chat, sender, sender_server, CAST(timestamp AS TEXT) AS timestamp, content AS text, is_from_me, quoted_message_id, target_message_id, media_type, filename, lower(hex(file_sha256)) AS sha256, CAST(deleted_at AS TEXT) AS deleted_at, view_once, location, mentions, media_presentation, message_edit_timestamp FROM messages ORDER BY chat_jid, id", "messages", messages, false},
			{"notes.jsonl", "SELECT target_type, target_id, key, value, updated_at, source, version FROM notes ORDER BY target_type, target_id, key, version", "notes", notes, false},
			{"media_notes.jsonl", "SELECT sha256, key, value, updated_at FROM media_notes WHERE key <> 'transcript' ORDER BY sha256, key", "media_notes", notes, false},
			{"transcriptions.jsonl", "SELECT sha256, value AS text, updated_at FROM media_notes WHERE key='transcript' ORDER BY sha256, key", "media_notes", notes, false},
		} {
			file := file
			if !exportTableExists(ctx, file.tx, file.table) {
				file.tx = nil
			}
			var transform func(map[string]any) error
			if file.name == "messages.jsonl" {
				transform = func(row map[string]any) error {
					for _, field := range []string{"location", "media_presentation"} {
						if raw, ok := row[field].(string); ok && json.Valid([]byte(raw)) {
							row[field] = json.RawMessage(raw)
						}
					}
					row["media_ref"] = nil
					if media {
						found, err := b.exportMedia(row)
						if err != nil {
							if errors.Is(err, errMediaPath) {
								if len(counts) > 0 {
									counts[0].skippedMedia++
								}
								return nil
							}
							return err
						}
						if found != nil {
							row["media_ref"] = "media/" + path.Join(chatMediaRel(row["chat"].(string)), found.name)
							found.Close()
						}
					}
					return nil
				}
			}
			if err := emit(archiveEntry{file.name, func(out io.Writer) (int64, error) {
				return exportRows(ctx, file.tx, file.query, out, file.array, transform)
			}}); err != nil {
				return err
			}
		}
		if !media {
			return nil
		}
		rows, err := messages.QueryContext(ctx, "SELECT id, chat_jid, media_type, CAST(timestamp AS TEXT), COALESCE(filename, '') FROM messages WHERE media_type IN ('image','video','audio','document','sticker') ORDER BY chat_jid, id")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, chat, kind, timestamp, filename any
			if err := rows.Scan(&id, &chat, &kind, &timestamp, &filename); err != nil {
				return err
			}
			found, err := b.exportMedia(map[string]any{"id": id, "chat": chat, "media_type": kind, "timestamp": timestamp, "filename": filename})
			if err != nil {
				if errors.Is(err, errMediaPath) {
					continue
				}
				return err
			}
			if found == nil {
				continue
			}
			err = emit(archiveEntry{"media/" + path.Join(chatMediaRel(chat.(string)), found.name), func(out io.Writer) (int64, error) {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				f, err := found.dir.Open(found.name)
				if err != nil {
					return 0, err
				}
				defer func() { _ = f.Close() }()
				info, err := f.Stat()
				if err != nil || !os.SameFile(found.info, info) {
					return 0, errors.New("cached media changed while opening")
				}
				_, err = copyArchiveContext(ctx, out, f)
				return 1, err
			}})
			found.Close()
			if err != nil {
				return err
			}
		}
		return rows.Err()
	}
}

func (b *Bridge) exportMedia(row map[string]any) (*cachedMedia, error) {
	kind, _ := row["media_type"].(string)
	switch kind {
	case "image", "video", "audio", "document", "sticker":
	default:
		return nil, nil
	}
	stamp, stampOK := row["timestamp"].(string)
	chat, chatOK := row["chat"].(string)
	id, idOK := row["id"].(string)
	if !stampOK || !chatOK || !idOK {
		return nil, errMediaPath
	}
	timestamp, err := parseDBTime(stamp)
	if err != nil {
		return nil, errMediaPath
	}
	filename, _ := row["filename"].(string)
	return findCachedMedia(b.StoreRoot, chatMediaRel(chat), mediaFileNames(kind, timestamp, id, filename))
}

func exportManifest(created time.Time, counts ...*exportCounts) func(io.Writer, archiveWalk) (int64, error) {
	return func(out io.Writer, walk archiveWalk) (int64, error) {
		prefix, err := json.Marshal(map[string]any{"version": buildInfo().Version, "created_at": created.UTC().Format(time.RFC3339)})
		if err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintf(out, "%s,\"files\":[", prefix[:len(prefix)-1]); err != nil {
			return 0, err
		}
		var count int64
		err = walk(func(entry archiveEntry) error {
			stats, err := measureEntry(entry, io.Discard, 0)
			if err != nil {
				return err
			}
			if count > 0 {
				if _, err := io.WriteString(out, ","); err != nil {
					return err
				}
			}
			count++
			return json.NewEncoder(out).Encode(map[string]any{"name": stats.name, "size": stats.size, "sha256": stats.sha, "count": stats.count})
		})
		if err != nil {
			return count, err
		}
		var skipped int64
		if len(counts) > 0 {
			skipped = counts[0].skippedMedia
		}
		_, err = fmt.Fprintf(out, "],\"media_skipped\":%d}\n", skipped)
		return count, err
	}
}

func (b *Bridge) handleExport() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		media, err := operatorBoolQuery(r, "media")
		if err != nil {
			writeError(w, 400, "Export accepts only media=true|false")
			return
		}
		if !b.exportBusy.CompareAndSwap(false, true) {
			writeError(w, 429, "An export is already running")
			return
		}
		defer b.exportBusy.Store(false)
		start := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), b.archiveTimeout())
		defer cancel()
		messages, err := openExportStore(ctx, b.StoreRoot, "messages.db", false)
		if err != nil {
			writeError(w, 503, "Export store unavailable")
			return
		}
		defer messages.close()
		notes, err := openExportStore(ctx, b.StoreRoot, "notes.db", true)
		if err != nil {
			writeError(w, 503, "Export notes unavailable")
			return
		}
		defer notes.close()
		var contacts exportStore
		if release, admitted := b.beginArchiveSessionRead(cancel); admitted {
			defer release()
			contacts, err = openExportStore(ctx, b.StoreRoot, "whatsapp.db", true)
			if err != nil {
				writeError(w, 503, "Export contacts unavailable")
				return
			}
		}
		defer contacts.close()
		// Override the pairing listener's short write deadline for large streams.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(b.archiveTimeout())); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeError(w, 500, "Export deadline unavailable")
			return
		}
		// Cancellation must also interrupt a client stalled inside a socket write,
		// allowing logout to join cleanup before destroying the session WAL.
		stop := context.AfterFunc(ctx, func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Now()) })
		defer stop()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="whatsapp-export.zip"`)
		counts := &exportCounts{}
		count, err := streamArchive(w, b.exportWalk(ctx, messages.tx, notes.tx, contacts.tx, media, counts), exportManifest(start, counts))
		if err != nil {
			b.Log.Warnf("Operator export failed after streaming began")
			panic(http.ErrAbortHandler)
		}
		b.Log.Infof("Operator export: files=%d media=%t duration_ms=%d", count, media, time.Since(start).Milliseconds())
	}
}

func (b *Bridge) operatorRoutes() operatorRoutes {
	routes := b.operatorPairing.routes()
	routes.export, routes.snapshot = b.handleExport(), b.handleSnapshot()
	return routes
}
