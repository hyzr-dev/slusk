package soulseek

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/file"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

func (c *Client) runUpload(ctx context.Context, job *uploadJob) {
	indexed := c.shareSnapshot().files[job.key.filename]
	if indexed == nil {
		c.denyQueuedUpload(ctx, job, peer.ErrFileNotShared)
		return
	}
	shared, err := openIndexedFile(indexed)
	if err != nil {
		c.denyQueuedUpload(ctx, job, peer.ErrFileReadError)
		return
	}
	defer shared.Close()

	session, err := c.getOrConnectPeerSession(ctx, job.key.username)
	if err != nil {
		return
	}
	reservation := c.tokens.Reserve()
	defer reservation.Release()
	responses := make(chan peer.TransferResponse, 1)
	c.uploads.registerToken(reservation.token, job.key.username, responses)
	defer c.uploads.unregisterToken(reservation.token, responses)

	request := &peer.TransferRequest{Direction: peer.UploadToPeer, Token: reservation.token, Filename: job.key.filename, FileSize: indexed.wire.Size}
	if !sendUploadPeerMessage(session, request) {
		return
	}

	timer := time.NewTimer(c.cfg.uploadNegotiationTimeout)
	defer timer.Stop()
	select {
	case response := <-responses:
		if !response.Allowed {
			return
		}
	case <-timer.C:
		return
	case <-ctx.Done():
		return
	}

	if err := c.streamUpload(ctx, job.key.username, reservation.token, shared, indexed.wire.Size); err != nil {
		_ = sendUploadPeerMessage(session, &peer.UploadFailed{Filename: job.key.filename})
		if c.logger != nil {
			c.logger.Debug("soulseek upload failed", "username", job.key.username, "filename", job.key.filename, "err", err)
		}
	}
}

func (c *Client) denyQueuedUpload(ctx context.Context, job *uploadJob, reason error) {
	session, err := c.getOrConnectPeerSession(ctx, job.key.username)
	if err == nil {
		_ = sendUploadPeerMessage(session, &peer.UploadDenied{Filename: job.key.filename, Reason: reason})
	}
}

func openIndexedFile(indexed *indexedFile) (*os.File, error) {
	resolvedBefore, err := filepath.EvalSymlinks(indexed.local)
	if err != nil {
		return nil, fmt.Errorf("resolve shared file before opening: %w", err)
	}
	resolvedBefore, err = filepath.Abs(resolvedBefore)
	if err != nil || !pathWithinRoot(indexed.root, resolvedBefore) {
		return nil, fmt.Errorf("shared file escaped resolved root")
	}
	before, err := os.Stat(resolvedBefore)
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(indexed.info, before) || before.Size() != int64(indexed.wire.Size) || !before.ModTime().Equal(indexed.info.ModTime()) {
		return nil, fmt.Errorf("shared file changed since scan")
	}
	f, err := os.Open(indexed.local)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		f.Close()
		return nil, fmt.Errorf("shared file changed while opening")
	}
	resolvedAfter, err := filepath.EvalSymlinks(indexed.local)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("resolve shared file after opening: %w", err)
	}
	resolvedAfter, err = filepath.Abs(resolvedAfter)
	if err != nil || !pathWithinRoot(indexed.root, resolvedAfter) {
		f.Close()
		return nil, fmt.Errorf("shared file escaped resolved root while opening")
	}
	after, err := os.Stat(resolvedAfter)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		f.Close()
		return nil, fmt.Errorf("shared file path changed while opening")
	}
	return f, nil
}

func (c *Client) streamUpload(ctx context.Context, username string, token soul.Token, shared *os.File, size uint64) error {
	conn, err := c.ConnectPeer(ctx, username, file.ConnectionType)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	return streamUploadConn(conn, token, shared, size, c.cfg.fileInitTimeout, c.cfg.fileIdleTimeout)
}

func streamUploadConn(conn net.Conn, token soul.Token, shared io.ReadSeeker, size uint64, initTimeout, idleTimeout time.Duration) error {
	if err := conn.SetWriteDeadline(time.Now().Add(idleTimeout)); err != nil {
		return err
	}
	if _, err := file.Write(conn, &file.TransferInit{Token: token}); err != nil {
		return fmt.Errorf("write transfer init: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(initTimeout)); err != nil {
		return err
	}
	offset := &file.Offset{}
	if err := offset.Deserialize(conn); err != nil {
		return fmt.Errorf("read upload offset: %w", err)
	}
	if offset.Offset > size {
		return fmt.Errorf("invalid upload offset %d greater than size %d", offset.Offset, size)
	}
	if offset.Offset < size {
		if _, err := shared.Seek(int64(offset.Offset), io.SeekStart); err != nil {
			return err
		}
		writer := &progressWriter{conn: conn, idleTimeout: idleTimeout}
		written, err := io.CopyN(writer, shared, int64(size-offset.Offset))
		if err != nil {
			return fmt.Errorf("stream upload (sent %d of %d): %w", written, size-offset.Offset, err)
		}
	}
	// The downloader owns successful F-connection completion. Keep the socket
	// open after the exact byte count and wait for it to close, bounded by the
	// same idle timeout used for transfer progress.
	if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
		if err == io.ErrClosedPipe {
			return nil
		}
		return err
	}
	var unexpected [1]byte
	n, err := conn.Read(unexpected[:])
	if (err == io.EOF || err == io.ErrClosedPipe) && n == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("wait for downloader to close completed upload: %w", err)
	}
	return fmt.Errorf("unexpected data after completed upload")
}

type progressWriter struct {
	conn        net.Conn
	idleTimeout time.Duration
}

func (w *progressWriter) Write(p []byte) (int, error) {
	if err := w.conn.SetWriteDeadline(time.Now().Add(w.idleTimeout)); err != nil {
		return 0, err
	}
	return w.conn.Write(p)
}
