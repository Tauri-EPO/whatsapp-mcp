package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestOutboundMediaRESTSendCacheDownloadPolicyAndFailures(t *testing.T) {
	for _, mode := range []string{"cached", "disabled", "at-cap", "over-cap", "write-failure", "symlink", "bad-id"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, rec)
			b.Connected = func() bool { return true }
			b.Send = b.sendBackend()
			b.MediaAutoDownload = mode != "disabled"
			const chat = "120363000000000001@g.us"
			stamp := time.Unix(1772359200, 0)
			data := []byte("synthetic outbound PDF bytes")
			file := filepath.Join(t.TempDir(), "sample.pdf")
			b.MediaRoots = []string{filepath.Dir(file)}
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "at-cap" {
				b.MediaMaxBytes = uint64(len(data))
			}
			if mode == "over-cap" {
				b.MediaMaxBytes = uint64(len(data)) - 1
			}
			id := "CACHE-SEND"
			if mode == "bad-id" {
				id = "../CACHE-SEND"
			}
			name := mediaFileName("document", stamp, id, "sample.pdf")
			outside := t.TempDir()
			if mode == "symlink" {
				if err := os.Symlink(outside, filepath.Join(storeDir(), chat)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "write-failure" {
				if err := b.StoreRoot.MkdirAll(path.Join(chat, name+".part", "occupied"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			uploads, sends, downloads := 0, 0, 0
			b.uploadMedia = func(_ context.Context, got []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				uploads++
				if !bytes.Equal(got, data) {
					t.Fatal("uploaded bytes changed")
				}
				up := outboundUpload()
				sum := sha256.Sum256(got)
				up.FileSHA256 = sum[:]
				up.FileLength = uint64(len(got))
				return up, nil
			}
			b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
				sends++
				return whatsmeow.SendResponse{ID: id, Timestamp: stamp}, nil
			}
			b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
				downloads++
				return 0, errors.New("network must not run on cache hit")
			}
			body, _ := json.Marshal(SendMessageRequest{Recipient: chat, Message: "caption", MediaPath: file})
			mux := b.newRESTMux(8080, sendRecipientToken)
			sent := httptest.NewRecorder()
			mux.ServeHTTP(sent, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
			var response SendMessageResponse
			if err := json.Unmarshal(sent.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if sent.Code != http.StatusOK || !response.Success || response.MessageID != id || sends != 1 || uploads != 1 {
				t.Fatalf("send=%d %+v sends=%d uploads=%d", sent.Code, response, sends, uploads)
			}
			// The MCP upload is removed after sending. The stored copy must stand alone.
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			cached, lookupErr := findCachedMedia(b.StoreRoot, chat, []string{name})
			shouldCache := mode == "cached" || mode == "at-cap"
			if shouldCache {
				if lookupErr != nil || cached == nil {
					t.Fatalf("sent bytes not cached: %v", lookupErr)
				}
				got, err := cached.dir.ReadFile(cached.name)
				cached.Close()
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("cached bytes=%q error=%v", got, err)
				}
				download := httptest.NewRecorder()
				payload, _ := json.Marshal(DownloadMediaRequest{MessageID: id, ChatJID: chat})
				mux.ServeHTTP(download, seamRequest(http.MethodPost, "/api/download", string(payload), sendRecipientToken))
				var fetched DownloadMediaResponse
				if err := json.Unmarshal(download.Body.Bytes(), &fetched); err != nil {
					t.Fatal(err)
				}
				if download.Code != http.StatusOK || !fetched.Success || downloads != 0 {
					t.Fatalf("download=%d %+v network=%d", download.Code, fetched, downloads)
				}
				got, err = os.ReadFile(fetched.Path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatal("download did not return original sent bytes")
				}
				if _, err := b.StoreRoot.Lstat(path.Join(chat, name+".part")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unfinished .part published: %v", err)
				}
			} else if cached != nil {
				cached.Close()
				t.Fatal("cache policy/deny path bypassed")
			}
			failed := mode == "write-failure" || mode == "symlink" || mode == "bad-id"
			wantWarn := 0
			if failed {
				wantWarn = 1
			}
			if strings.Count(rec.String(), "Sent media cache failed") != wantWarn || b.metrics.storeFailures.Load() != 0 || b.metrics.sendFailures.Load() != 0 {
				t.Fatalf("failure changed send result: %s", rec.String())
			}
			if mode == "over-cap" && b.metrics.mediaAutoSizeSkips.Load() != 1 {
				t.Fatal("size skip not counted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("store symlink wrote outside root")
			}
		})
	}
}

func TestOutboundMediaCachesDespiteArchiveFailure(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, installRecordingLogger(t))
	b.MediaAutoDownload = true
	b.Connected = func() bool { return true }
	const chat = "120363000000000001@g.us"
	stamp := time.Unix(1772359200, 0)
	file := filepath.Join(t.TempDir(), "sample.pdf")
	data := []byte("synthetic sent bytes")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec("CREATE TRIGGER reject_send BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'archive unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		return outboundUpload(), nil
	}
	b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
		return whatsmeow.SendResponse{ID: "CACHE-FAILED-ROW", Timestamp: stamp}, nil
	}
	ok, status, sent := b.sendBackend()(context.Background(), chat, "caption", file, "", "", "", nil)
	if !ok || !strings.Contains(status, "archive row could not be written") || b.metrics.storeFailures.Load() != 1 {
		t.Fatalf("send=%v status=%s failures=%d", ok, status, b.metrics.storeFailures.Load())
	}
	cached, err := findCachedMedia(b.StoreRoot, chat, []string{mediaFileName("document", stamp, sent.ID, "sample.pdf")})
	if err != nil || cached == nil {
		t.Fatalf("successful send discarded bytes on archive failure: %v", err)
	}
	defer cached.Close()
	got, err := cached.dir.ReadFile(cached.name)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("saved bytes=%q error=%v", got, err)
	}
}

func TestOutboundCacheWaitEndsAtSendDeadlineWithoutCancellingTransfer(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, installRecordingLogger(t))
	b.MediaAutoDownload = true
	b.Connected = func() bool { return true }
	const chat = "120363000000000001@g.us"
	stamp := time.Unix(1772359200, 0)
	data := []byte("synthetic uploaded bytes")
	file := filepath.Join(t.TempDir(), "sample.pdf")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		return outboundUpload(), nil
	}
	b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
		return whatsmeow.SendResponse{ID: "BOUND-CACHE", Timestamp: stamp}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, transferDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	persist := func(storage types.JID, sent sentMessage, content string, media outboundMedia, quote string) (string, error) {
		stored, err := b.persistOutbound(storage, sent, content, media, quote)
		if err != nil {
			return stored, err
		}
		name := mediaFileName(media.mediaType, sent.Timestamp, sent.ID, media.filename)
		absolute, err := filepath.Abs(storePath(chat, name))
		if err != nil {
			return stored, err
		}
		go func() {
			defer close(transferDone)
			_, _ = b.mediaTransfers.do(b.ctx, absolute, func() (int64, error) {
				close(started)
				<-release
				if err := b.ctx.Err(); err != nil {
					return 0, err
				}
				if err := b.StoreRoot.MkdirAll(chat, 0o700); err != nil {
					return 0, err
				}
				return writeMediaFile(b.StoreRoot, path.Join(chat, name), func(f *os.File) error { _, err := f.Write(data); return err })
			})
		}()
		<-started // a manual transfer owns this path before the send's cache call
		cancel()
		return stored, nil
	}
	sendDone := make(chan struct{})
	var ok bool
	var sent sentMessage
	go func() {
		defer close(sendDone)
		ok, _, sent = b.sendWhatsAppMessage(ctx, persist, chat, "caption", file, "", "", "", nil)
	}()
	select {
	case <-sendDone:
	case <-time.After(time.Second):
		t.Fatal("cache wait outlived the send context")
	}
	if !ok || sent.ID != "BOUND-CACHE" {
		t.Fatalf("acknowledged send lost success: %v %+v", ok, sent)
	}
	select {
	case <-transferDone:
		t.Fatal("send cancellation killed the shared transfer")
	default:
	}
	close(release)
	select {
	case <-transferDone:
	case <-time.After(time.Second):
		t.Fatal("transfer did not finish after release")
	}
	cached, err := findCachedMedia(b.StoreRoot, chat, []string{mediaFileName("document", stamp, sent.ID, "sample.pdf")})
	if err != nil || cached == nil {
		t.Fatalf("lifecycle transfer lost its file: %v", err)
	}
	defer cached.Close()
	got, err := cached.dir.ReadFile(cached.name)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("shared transfer bytes changed")
	}
}

func TestOutboundCacheRecoversFailedDownloadJoinAfterCallerLeaves(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-request=%v", cancelRequest), func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, installRecordingLogger(t))
			b.MediaAutoDownload = true
			b.Connected = func() bool { return true }
			const chat = "120363000000000001@g.us"
			stamp := time.Unix(1772359200, 0)
			data := []byte("synthetic successful upload")
			file := filepath.Join(t.TempDir(), "sample.pdf")
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				return outboundUpload(), nil
			}
			b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
				return whatsmeow.SendResponse{ID: "FAILED-JOIN", Timestamp: stamp}, nil
			}
			name := mediaFileName("document", stamp, "FAILED-JOIN", "sample.pdf")
			absolute, err := filepath.Abs(storePath(chat, name))
			if err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			persist := func(storage types.JID, sent sentMessage, content string, media outboundMedia, quote string) (string, error) {
				stored, err := b.persistOutbound(storage, sent, content, media, quote)
				if err != nil {
					return stored, err
				}
				go func() {
					_, _ = b.mediaTransfers.do(b.ctx, absolute, func() (int64, error) {
						close(started)
						<-release
						if err := b.StoreRoot.MkdirAll(chat, 0o700); err != nil {
							return 0, err
						}
						return writeMediaFile(b.StoreRoot, path.Join(chat, name), func(f *os.File) error {
							if _, err := f.Write([]byte("partial CDN bytes")); err != nil {
								return err
							}
							return errors.New("synthetic CDN failure")
						})
					})
				}()
				<-started
				return stored, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sendDone := make(chan bool, 1)
			go func() {
				ok, _, _ := b.sendWhatsAppMessage(ctx, persist, chat, "caption", file, "", "", "", nil)
				sendDone <- ok
			}()
			awaitCallers(t, &b.mediaTransfers, absolute, 2)
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if cancelRequest {
				cancel()
				select {
				case ok := <-sendDone:
					if !ok {
						t.Fatal("acknowledged send failed")
					}
				case <-time.After(time.Second):
					t.Fatal("cache wait ignored request cancellation")
				}
			}
			close(release)
			if !cancelRequest {
				select {
				case ok := <-sendDone:
					if !ok {
						t.Fatal("failed download changed send success")
					}
				case <-time.After(time.Second):
					t.Fatal("cache fallback did not complete")
				}
			}
			b.mediaTransfers.wait()
			cached, err := findCachedMedia(b.StoreRoot, chat, []string{name})
			if err != nil || cached == nil {
				t.Fatalf("failed joined download lost uploaded bytes: %v", err)
			}
			defer cached.Close()
			got, err := cached.dir.ReadFile(cached.name)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("cache contains partial download bytes: %q err=%v", got, err)
			}
			if _, err := b.StoreRoot.Stat(path.Join(chat, name+".part")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial file left behind: %v", err)
			}
		})
	}
}
