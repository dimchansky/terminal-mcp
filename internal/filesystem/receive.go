package filesystem

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A received file stays out of MCP payloads and the model context. Limit the
// disk space a single authenticated tool call can consume, not its file format.
const MaxReceiveBytes int64 = 128 << 20
const receiveTimeout = 3 * time.Minute

type fileReceiver struct {
	client *http.Client
}

func newFileReceiver() *fileReceiver {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A Content-Encoding transformation would change the bytes being saved.
	transport.DisableCompression = true
	return &fileReceiver{client: &http.Client{Transport: transport, Timeout: receiveTimeout}}
}

// parseDownloadURL checks only what net/http needs to fetch a file.
// Hosts, ports and networks are unrestricted, just like terminal_start.
func parseDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("file.download_url must be an absolute HTTP or HTTPS URL")
	}
	return u, nil
}

func (m *Manager) Receive(ctx context.Context, a ReceiveArgs) (ActionResult, error) {
	rel, err := m.relative(a.Path)
	if err != nil {
		return ActionResult{}, err
	}
	if a.File.FileID == "" || len(a.File.FileID) > 512 {
		return ActionResult{}, errors.New("file.file_id must be nonempty and at most 512 bytes")
	}
	u, err := parseDownloadURL(a.File.DownloadURL)
	if err != nil {
		return ActionResult{}, err
	}
	// Keep retries bound to the original file identity and URL. No URL is
	// persisted by the ledger: only its digest contributes to the fingerprint.
	effect := a
	effect.ServerID, effect.OperationID = "", ""
	return m.mutateUnlocked(ctx, a.ServerID, a.OperationID, "file_receive", effect, func() (FileReceipt, error) {
		return m.receiveFile(rel, a.Path, u)
	})
}

// The download happens outside Manager.mu; only final publication takes the
// lock. Mutations already admitted by the ledger finish even if the requesting
// client's observation times out or disconnects.
func (m *Manager) receiveFile(rel, path string, source *url.URL) (FileReceipt, error) {
	receipt := FileReceipt{Path: path}
	parent, err := m.root.OpenRoot(filepath.Dir(rel))
	if err != nil {
		return receipt, err
	}
	defer func() { _ = parent.Close() }()
	name := filepath.Base(rel)
	if _, err := parent.Lstat(name); err == nil {
		return receipt, errors.New("file_exists: file_receive never overwrites an existing destination")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return receipt, err
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return receipt, err
	}
	temp := ".terminal-mcp-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := parent.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return receipt, err
	}
	defer func() {
		_ = f.Close()
		if err := parent.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Error("filesystem: failed to remove received-file temporary file", "error", err)
		}
	}()

	// The ledger owns the effect lifetime. A bounded independent deadline lets
	// an accepted transfer complete after an MCP client request is cancelled.
	downloadCtx, cancel := context.WithTimeout(context.Background(), receiveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, source.String(), nil)
	if err != nil {
		return receipt, errors.New("cannot construct file download request")
	}
	client := *m.receiver.client
	client.Timeout = receiveTimeout
	resp, err := client.Do(req)
	if err != nil {
		// Avoid repeating the URL in the tool's error text: the complete
		// signed URL is already captured in the owner's request_start audit.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return receipt, fmt.Errorf("file download failed: %v", urlErr.Err)
		}
		return receipt, errors.New("file download failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return receipt, fmt.Errorf("file download returned HTTP %d", resp.StatusCode)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return receipt, errors.New("file download has unexpected Content-Encoding")
	}
	if resp.ContentLength > MaxReceiveBytes {
		return receipt, fmt.Errorf("file exceeds %d byte download limit", MaxReceiveBytes)
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, MaxReceiveBytes+1))
	if err != nil {
		return receipt, fmt.Errorf("file download stream failed: %w", err)
	}
	if count > MaxReceiveBytes {
		return receipt, fmt.Errorf("file exceeds %d byte download limit", MaxReceiveBytes)
	}
	if err := f.Sync(); err != nil {
		return receipt, err
	}
	if err := f.Close(); err != nil {
		return receipt, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return receipt, errors.New("file service is closed")
	}
	// Link publishes a fully written regular file, and never replaces a
	// destination another process created during the download.
	if err := parent.Link(temp, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return receipt, errors.New("file_exists: destination created during download")
		}
		return receipt, err
	}
	receipt.Bytes = int(count)
	receipt.Version = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return receipt, nil
}
