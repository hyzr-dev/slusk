package soulseek

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/file"
)

// progressReader wraps an F connection's socket for reading file data,
// resetting the read deadline to now+idleTimeout on every call. This is a
// progress-based idle timeout, analogous to sessionDeadlineReader in
// sessions.go: it bounds silence between reads, not the transfer's total
// duration, so a slow but steady peer sending a large file is never cut off.
type progressReader struct {
	conn        net.Conn
	idleTimeout time.Duration
}

func (r *progressReader) Read(p []byte) (int, error) {
	if err := r.conn.SetReadDeadline(time.Now().Add(r.idleTimeout)); err != nil {
		return 0, err
	}
	return r.conn.Read(p)
}

// readerFunc adapts a plain function to io.Reader, used by streamFile to tee
// bytes read off the wire into a progress callback without a second buffer
// copy.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// handleInboundFileConn completes the handshake on a freshly accepted
// (listener.go handlePeerConn) or mirror-dialed (peers.go handleConnectToPeer)
// F connection: read the peer's TransferInit - a raw 4-byte token, with none
// of the size+code framing ordinary P/D messages carry - match it against a
// transfer waiting for it, and hand the socket off. ctx is accepted for
// symmetry with the package's other handleXxx entry points and to leave room
// for cancellation later; the handshake read is already deadline-bounded, so
// nothing currently needs it.
//
// conn and lease (nil on the outgoing/mirror-dial path, which holds no
// inbound lease) are closed/released here whenever the handshake fails or
// nothing claims the socket. Ownership passes to whichever orchestration
// goroutine is waiting on the matched transfer's fileConnCh (Group E) only
// once attachFileConn reports success; from that point on this function does
// not touch conn or lease again.
func (c *Client) handleInboundFileConn(ctx context.Context, conn net.Conn, lease *inboundLease) {
	if err := conn.SetReadDeadline(time.Now().Add(c.cfg.fileInitTimeout)); err != nil {
		_ = conn.Close()
		lease.Release()
		return
	}

	ti := &file.TransferInit{}
	if err := ti.Deserialize(conn); err != nil {
		if c.logger != nil {
			c.logger.Debug("read file transfer init", "remote", conn.RemoteAddr(), "err", err)
		}
		_ = conn.Close()
		lease.Release()
		return
	}

	tr := c.downloads.claimByToken(ti.Token)
	if tr == nil {
		if c.logger != nil {
			c.logger.Debug("file connection for unknown or already-claimed token", "remote", conn.RemoteAddr(), "token", ti.Token)
		}
		_ = conn.Close()
		lease.Release()
		return
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		lease.Release()
		return
	}

	if !tr.attachFileConn(conn, lease) {
		// Nothing is (or is still) waiting on tr.fileConnCh - e.g. the
		// download was cancelled between negotiation and this handoff, or a
		// second TransferInit arrived for a token already delivered once.
		_ = conn.Close()
		lease.Release()
	}
}

// streamFile copies size bytes of file data from conn to destPath, resuming
// from a same-named ".part" file if one already exists, and renaming it into
// place atomically once every byte has arrived. It owns the F connection
// handshake beyond TransferInit: it sends the Offset message itself, so the
// caller must not read or write conn before or after calling streamFile.
//
// progress, if non-nil, is called after every successful read with the
// file's cumulative byte count now on disk (resumeOffset plus bytes copied so
// far this call) - not just the bytes copied by this call alone - so a
// caller can drive a transfer's bytesDone directly from it as the download
// proceeds, rather than only once at the end.
//
// On any error the ".part" file is left in place with whatever bytes had
// already landed, so a retried streamFile call resumes from where this one
// left off; only a fully successful transfer is renamed to destPath.
func streamFile(conn net.Conn, destPath string, size int64, idleTimeout time.Duration, progress func(written int64)) (written int64, err error) {
	// Defense in depth: a negative size (e.g. a peer FileSize that overflowed
	// int64) must never reach io.CopyN, where a negative count is a silent
	// no-op that would "complete" a 0-byte file. runDownload rejects this
	// upstream; guard the primitive too so it can never happen through any path.
	if size < 0 {
		return 0, fmt.Errorf("refusing to stream a negative transfer size %d", size)
	}
	partPath := destPath + ".part"

	resumeOffset, partExists, err := partialFileSize(partPath)
	if err != nil {
		return 0, err
	}
	if resumeOffset > size {
		// A stale .part longer than the file we now expect - e.g. a
		// different, larger upload of the same destination path was
		// interrupted here previously. It cannot be resumed from; discard it
		// and start over from 0.
		if rmErr := os.Remove(partPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return 0, fmt.Errorf("discard oversized partial download %s: %w", partPath, rmErr)
		}
		resumeOffset = 0
		partExists = false
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return 0, fmt.Errorf("create destination directory for %s: %w", destPath, err)
	}

	if partExists && resumeOffset == size {
		// A prior attempt already wrote every byte to the .part file but was
		// interrupted before the final rename. Finish that rename without
		// touching conn at all - this remains valid for a completed empty
		// partial too. A fresh zero-byte download has no partial, so it must
		// instead perform the Offset(0) handshake below.
		if renameErr := os.Rename(partPath, destPath); renameErr != nil {
			return 0, fmt.Errorf("rename completed partial download %s: %w", partPath, renameErr)
		}
		return size, nil
	}

	if _, err := file.Write(conn, &file.Offset{Offset: uint64(resumeOffset)}); err != nil {
		return 0, fmt.Errorf("write resume offset to peer: %w", err)
	}

	part, err := os.OpenFile(partPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open partial download %s: %w", partPath, err)
	}

	reader := &progressReader{conn: conn, idleTimeout: idleTimeout}
	counted := int64(0)
	tee := readerFunc(func(p []byte) (int, error) {
		n, rerr := reader.Read(p)
		if n > 0 {
			counted += int64(n)
			if progress != nil {
				progress(resumeOffset + counted)
			}
		}
		return n, rerr
	})

	n, copyErr := io.CopyN(part, tee, size-resumeOffset)
	written = resumeOffset + n
	if copyErr != nil {
		_ = part.Close()
		return written, fmt.Errorf("stream file data (got %d of %d remaining bytes): %w", n, size-resumeOffset, copyErr)
	}

	if err := part.Sync(); err != nil {
		_ = part.Close()
		return written, fmt.Errorf("sync partial download %s: %w", partPath, err)
	}
	if err := part.Close(); err != nil {
		return written, fmt.Errorf("close partial download %s: %w", partPath, err)
	}
	if err := os.Rename(partPath, destPath); err != nil {
		return written, fmt.Errorf("rename completed download %s: %w", partPath, err)
	}
	return written, nil
}

// partialFileSize returns the size and existence of a ".part" file. The
// existence bit distinguishes a completed empty partial from a fresh
// zero-byte download, whose required Offset(0) handshake must not be skipped.
func partialFileSize(path string) (size int64, exists bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("stat partial download %s: %w", path, err)
	}
	return info.Size(), true, nil
}
