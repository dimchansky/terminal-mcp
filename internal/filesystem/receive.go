package filesystem

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"terminalmcp/internal/mutation"
)

type fileReceiver struct {
	client *http.Client
}

func newFileReceiver() *fileReceiver {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Saving a binary file must never change its compressed response bytes.
	transport.DisableCompression = true
	// No overall transfer deadline: downloads can outlive MCP tool calls.
	// Only an explicit forced stop cancels the shared transfer context.
	return &fileReceiver{client: &http.Client{Transport: transport}}
}

func parseDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("file.download_url must be an absolute HTTP or HTTPS URL")
	}
	return u, nil
}

// Observation waits are shorter than the HTTP server's 30-second write deadline.
// They never constrain the lifetime of the independently running transfer.
const (
	defaultReceiveWaitMS = 2000
	defaultResultWaitMS  = 20000
	maxReceiveWaitMS     = 20000
)

func receiveWaitMS(requested *int, fallback int) (time.Duration, error) {
	ms := fallback
	if requested != nil {
		ms = *requested
	}
	if ms < 0 || ms > maxReceiveWaitMS {
		return 0, fmt.Errorf("wait_ms must be between 0 and %d", maxReceiveWaitMS)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// Receive admits a download after the pre-effect audit, optionally observing
// completion for a bounded time. Its wait_ms is not part of the retry identity.
func (m *Manager) Receive(ctx context.Context, a ReceiveArgs) (ReceiveResult, error) {
	wait, err := receiveWaitMS(a.WaitMS, defaultReceiveWaitMS)
	if err != nil {
		return ReceiveResult{}, err
	}
	rel, err := m.relative(a.Path)
	if err != nil {
		return ReceiveResult{}, err
	}
	if a.File.FileID == "" || len(a.File.FileID) > 512 {
		return ReceiveResult{}, errors.New("file.file_id must be nonempty and at most 512 bytes")
	}
	u, err := parseDownloadURL(a.File.DownloadURL)
	if err != nil {
		return ReceiveResult{}, err
	}
	effect := a
	effect.ServerID, effect.OperationID, effect.WaitMS = "", "", nil
	snapshot, replayed, err := m.host.StartMutation(ctx, a.ServerID, a.OperationID, "file_receive", effect,
		func(report func(mutation.Progress)) mutation.Receipt {
			receipt, receiveErr := m.receiveFile(rel, a.Path, u, report)
			status, message := "applied", ""
			if receiveErr != nil {
				status, message = "failed", receiveErr.Error()
			}
			data, err := json.Marshal(receipt)
			if err != nil {
				panic(err)
			}
			return mutation.Receipt{Status: status, Error: message, Data: data}
		})
	if err != nil {
		return ReceiveResult{}, err
	}
	if wait > 0 {
		snapshot, err = m.host.WaitMutation(ctx, a.ServerID, a.OperationID, "file_receive", wait)
		if err != nil {
			return ReceiveResult{}, err
		}
	}
	out, err := receiveResult(snapshot)
	if err != nil {
		return ReceiveResult{}, err
	}
	out.Replayed = &replayed
	return out, nil
}

// ReceiveResult waits for a previously admitted transfer, never restarting it.
// Completed receipts remain available until this server instance exits.
func (m *Manager) ReceiveResult(ctx context.Context, a ReceiveResultArgs) (ReceiveResult, error) {
	wait, err := receiveWaitMS(a.WaitMS, defaultResultWaitMS)
	if err != nil {
		return ReceiveResult{}, err
	}
	snapshot, err := m.host.WaitMutation(ctx, a.ServerID, a.OperationID, "file_receive", wait)
	if err != nil {
		return ReceiveResult{}, err
	}
	return receiveResult(snapshot)
}

func receiveResult(snapshot mutation.Snapshot) (ReceiveResult, error) {
	p := snapshot.Progress
	out := ReceiveResult{
		OperationID: snapshot.OperationID, Status: snapshot.Status,
		Stage: p.Stage, Path: p.Path,
		BytesDownloaded: p.BytesDownloaded, TotalBytes: p.TotalBytes,
		StartedAt: snapshot.StartedAt, CompletedAt: snapshot.CompletedAt,
	}
	if p.TotalBytes != nil && *p.TotalBytes > 0 {
		percent := 100 * float64(p.BytesDownloaded) / float64(*p.TotalBytes)
		if percent > 100 {
			percent = 100
		}
		out.Percent = &percent
	}
	if snapshot.Receipt != nil {
		var receipt FileReceipt
		if len(snapshot.Receipt.Data) != 0 {
			if err := json.Unmarshal(snapshot.Receipt.Data, &receipt); err != nil {
				return ReceiveResult{}, err
			}
		}
		receipt.OperationID, receipt.Status, receipt.Error =
			snapshot.OperationID, snapshot.Receipt.Status, snapshot.Receipt.Error
		out.Receipt = &receipt
		if receipt.Path != "" {
			out.Path = receipt.Path
		}
		if snapshot.Status == "completed" {
			total := int64(receipt.Bytes)
			percent := float64(100)
			out.BytesDownloaded, out.TotalBytes, out.Percent = total, &total, &percent
		}
	}
	return out, nil
}

type receiveProgressWriter struct {
	writer io.Writer
	count  int64
	path   string
	total  *int64
	report func(mutation.Progress)
}

func (w *receiveProgressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.count += int64(n)
	w.report(mutation.Progress{
		Stage: "downloading", Path: w.path, BytesDownloaded: w.count, TotalBytes: w.total,
	})
	return n, err
}

// The transfer runs independently of the MCP request context. Shutdown
// draining waits for it; only explicit stop --force cancels m.receiveCtx.
func (m *Manager) receiveFile(rel, path string, source *url.URL, report func(mutation.Progress)) (FileReceipt, error) {
	receipt := FileReceipt{Path: path}
	report(mutation.Progress{Stage: "connecting", Path: path})
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

	req, err := http.NewRequestWithContext(m.receiveCtx, http.MethodGet, source.String(), nil)
	if err != nil {
		return receipt, errors.New("cannot construct file download request")
	}
	resp, err := m.receiver.client.Do(req)
	if err != nil {
		// The full signed URL is already available to the owner in request_start.
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
	var total *int64
	if resp.ContentLength >= 0 {
		size := resp.ContentLength
		total = &size
	}
	report(mutation.Progress{Stage: "downloading", Path: path, TotalBytes: total})
	hash := sha256.New()
	progress := &receiveProgressWriter{writer: io.MultiWriter(f, hash), path: path, total: total, report: report}
	count, err := io.Copy(progress, resp.Body)
	if err != nil {
		return receipt, fmt.Errorf("file download stream failed: %w", err)
	}
	report(mutation.Progress{Stage: "finalizing", Path: path, BytesDownloaded: count, TotalBytes: total})
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
	// Fully written regular file is published atomically without overwriting.
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
