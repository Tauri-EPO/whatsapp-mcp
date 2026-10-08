package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
)

// Real SDK HTTP -> encrypted hash/MAC -> in-place CBC decryption -> file.
// Only media-connection discovery requires pairing, so its internal entrypoint
// is used with a local HTTP server; production still calls DownloadToFile.
func TestAutomaticCapWithRealSDKEncryptedHTTPDownload(t *testing.T) {
	for _, length := range []int{0, 16, 17, 1024} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("plain=%d/chunked=%v", length, chunked), func(t *testing.T) {
				plain := bytes.Repeat([]byte("x"), length)
				mediaKey := bytes.Repeat([]byte{2}, 32)
				expanded := hkdfutil.SHA256(mediaKey, nil, []byte(whatsmeow.MediaDocument), 112)
				iv, key, macKey := expanded[:16], expanded[16:48], expanded[48:80]
				cipher, err := cbcutil.Encrypt(key, iv, plain)
				if err != nil {
					t.Fatal(err)
				}
				mac := hmac.New(sha256.New, macKey)
				_, _ = mac.Write(iv)
				_, _ = mac.Write(cipher)
				wire := append(cipher, mac.Sum(nil)[:10]...)
				plainHash, wireHash := sha256.Sum256(plain), sha256.Sum256(wire)
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if chunked {
						w.(http.Flusher).Flush()
					}
					_, _ = w.Write(wire)
				}))
				t.Cleanup(srv.Close)
				client := newTestClient(&mockLIDStore{})
				client.SetMediaHTTPClient(srv.Client())
				root := storeRootAt(t, t.TempDir())
				written, err := writeMediaDownload(withMediaLimit(t.Context(), 16), root, "cached.bin", func(ctx context.Context, file whatsmeow.File) error {
					//nolint:staticcheck // test-only SDK HTTP/decryption entrypoint bypasses pairing.
					return client.DangerousInternals().DownloadAndDecryptToFile(ctx, srv.URL, mediaKey, whatsmeow.MediaDocument, wireHash[:], plainHash[:], file)
				})
				if length > 16 {
					if !errors.Is(err, errAutoMediaLimit) {
						t.Fatalf("oversized SDK download: written=%d err=%v", written, err)
					}
					if _, err := root.Stat("cached.bin"); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("oversized cache published: %v", err)
					}
				} else {
					if err != nil || written != int64(length) {
						t.Fatalf("valid SDK download: written=%d err=%v", written, err)
					}
					file, err := root.Open("cached.bin")
					if err != nil {
						t.Fatal(err)
					}
					cached, readErr := io.ReadAll(file)
					_ = file.Close()
					if readErr != nil || !bytes.Equal(cached, plain) {
						t.Fatalf("decrypted cache mismatch: %v", readErr)
					}
				}
				if requests.Load() != 1 {
					t.Fatalf("cap failure retried: requests=%d", requests.Load())
				}
				if _, err := root.Stat("cached.bin.part"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("SDK temporary file left: %v", err)
				}
			})
		}
	}
}
