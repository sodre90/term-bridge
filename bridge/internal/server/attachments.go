package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sodre90/cmux-bridge/internal/wire"
)

// AttachmentStore lands images a phone sends over the terminal socket as
// files the agent in the pane can be pointed at (cmux-app-ej0). cmux has no
// image RPC, so a file on disk plus its path pasted into the pane is the
// whole delivery mechanism.
//
// The phone never names a file: every path is the store's directory, a
// timestamp, six random hex digits and an extension chosen from the bytes
// themselves. Anything that does not sniff as an image the store refuses
// before it touches the disk.
type AttachmentStore struct {
	dir       string
	maxBytes  int
	retention time.Duration
	now       func() time.Time
}

const (
	// attachmentMaxBytes bounds one image. The phone downscales to a few
	// hundred KB by default and greys out "send original" above this, so
	// the bridge refusing here is the backstop, not the normal path.
	attachmentMaxBytes = 10 << 20
	// attachmentRetention is how long a landed file stays before Sweep
	// removes it: long enough to still be there in a conversation the user
	// comes back to, short enough that the directory does not become an
	// archive of everything ever sent.
	attachmentRetention = 7 * 24 * time.Hour
)

func NewAttachmentStore(dir string) *AttachmentStore {
	return &AttachmentStore{dir: dir, maxBytes: attachmentMaxBytes, retention: attachmentRetention, now: time.Now}
}

var (
	errAttachmentEmpty       = errors.New("attachment: empty")
	errAttachmentTooLarge    = errors.New("attachment: over size limit")
	errAttachmentNotImage    = errors.New("attachment: not a recognised image")
	errAttachmentsOff        = errors.New("attachment: no store configured")
	errAttachmentBadEncoding = errors.New("attachment: not base64")
)

// attachRefusalReason names, for the phone, why the bridge itself refused an
// attachment. cmux failing the paste gets no reason, like any other RPC
// failure, so the app's existing "delayed" handling covers it unchanged.
func attachRefusalReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errAttachmentTooLarge):
		return "too_large"
	case errors.Is(err, errAttachmentNotImage), errors.Is(err, errAttachmentEmpty):
		return "not_image"
	case errors.Is(err, errAttachmentsOff):
		return "attachments_off"
	case errors.Is(err, errAttachmentBadEncoding):
		return "bad_encoding"
	}
	return ""
}

// attachImage lands the image in an "attach" frame and pastes its path into
// the pane, followed by a space so the user can carry on typing. The error
// is what the frame's ack reports. The log gets the path, the size and the
// hint; the bytes never (invariant 5).
func (s *Server) attachImage(ctx context.Context, surfaceID string, up wire.TerminalUp) error {
	if s.attachments == nil {
		return errAttachmentsOff
	}
	if len(up.Image) > base64.StdEncoding.EncodedLen(s.attachments.maxBytes) {
		slog.Warn("terminal: attachment refused", "surface_id", surfaceID, "encoded_bytes", len(up.Image), "err", errAttachmentTooLarge)
		return errAttachmentTooLarge
	}
	image, err := base64.StdEncoding.DecodeString(up.Image)
	if err != nil {
		slog.Warn("terminal: attachment refused", "surface_id", surfaceID, "err", errAttachmentBadEncoding)
		return errAttachmentBadEncoding
	}
	path, err := s.attachments.Save(image)
	if err != nil {
		slog.Warn("terminal: attachment refused", "surface_id", surfaceID, "bytes", len(image), "err", err)
		return err
	}
	slog.Info("terminal: attachment landed", "surface_id", surfaceID, "path", path, "bytes", len(image), "hint", up.Name)
	_, err = s.cmux.Rpc(ctx, "mobile.terminal.paste",
		map[string]any{"surface_id": surfaceID, "text": path + " "})
	return err
}

// Save writes image to a new file and returns its path. The directory is
// created on first use, private to the user; the file is written under a
// temporary name and renamed into place so a half-written file is never
// visible at the path that gets pasted.
func (s *AttachmentStore) Save(image []byte) (string, error) {
	if len(image) == 0 {
		return "", errAttachmentEmpty
	}
	if len(image) > s.maxBytes {
		return "", errAttachmentTooLarge
	}
	ext, ok := sniffImageExtension(image)
	if !ok {
		return "", errAttachmentNotImage
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.dir, ".partial-*")
	if err != nil {
		return "", err
	}
	discard := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(image); err != nil {
		discard()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		discard()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		discard()
		return "", err
	}
	path := filepath.Join(s.dir, s.freshName(ext))
	if err := os.Rename(tmp.Name(), path); err != nil {
		discard()
		return "", err
	}
	return path, nil
}

func (s *AttachmentStore) freshName(ext string) string {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		panic(err) // crypto/rand failing is not a condition to carry on under
	}
	return s.now().Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:]) + "." + ext
}

// Sweep removes files in the store older than the retention. It only ever
// looks at regular files directly in the directory -- never below it, never
// anywhere else -- and a directory that does not exist yet is nothing to do.
// Returns how many files it removed.
func (s *AttachmentStore) Sweep() (int, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-s.retention)
	removed := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// sniffImageExtension identifies the image formats the store accepts by their
// leading bytes and returns the extension the file gets. The phone's own
// naming is deliberately not consulted: the bytes are the only thing that
// cannot lie about what the agent will open.
func sniffImageExtension(b []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg", true
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png", true
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "gif", true
	case len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp", true
	case isHEIF(b):
		return "heic", true
	}
	return "", false
}

// isHEIF recognises the ISO base media file type box with one of the brands
// Apple's camera writes. The brand is at bytes 8-12, after the box length
// and the literal "ftyp".
func isHEIF(b []byte) bool {
	if len(b) < 12 || !bytes.Equal(b[4:8], []byte("ftyp")) {
		return false
	}
	switch string(b[8:12]) {
	case "heic", "heix", "hevc", "hevx", "mif1", "msf1", "heif":
		return true
	}
	return false
}
