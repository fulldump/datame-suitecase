package takeout

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"path"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Gmail in Takeout is one big mbox (all the mail, tens of GB for some
// people). It is kept as it comes, and also split into one .eml per message,
// which any mail program opens, laid out like a copy by IMAP:
// Correo/<label>/<AAAA>/<AAAA-MM>/<AAAA-MM-DD_HHMMSS>-<id>.eml.
//
// A message is saved once, by its Message-ID: the cursor remembers which ones
// are in the NAS, so a message that changes labels from one export to the
// next (archived, labelled later) is not saved again in another folder.

func (x *importer) mbox(ctx context.Context, e *entry, rel string) error {
	if x.run.Config.Get("keep_mbox") != "false" {
		if _, err := x.raw(ctx, e, "Correo/mbox/"+cleanName(path.Base(rel)), ""); err != nil {
			return err
		}
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	defer r.Close()
	x.run.Logf("Separando %s en un fichero por correo", path.Base(rel))
	return splitMbox(r, x.tmp, func(m *spool, from string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return x.message(ctx, m, from)
	})
}

// splitMbox reads an mbox as a stream and calls fn with each message. A
// message starts at a line that begins with «From »; Gmail writes the ones in
// the text as «>From », so there is no doubt. Lines may be of any length.
func splitMbox(r io.Reader, tmp string, fn func(*spool, string) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var current *spool
	from := ""
	flush := func() error {
		if current == nil {
			return nil
		}
		err := fn(current, from)
		current.Close()
		current = nil
		return err
	}
	lineStart := true
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if lineStart && bytes.HasPrefix(chunk, []byte("From ")) {
				if err := flush(); err != nil {
					return err
				}
				current = &spool{dir: tmp}
				from = strings.TrimSpace(string(chunk[5:]))
			} else if current != nil {
				if _, err := current.Write(chunk); err != nil {
					current.Close()
					return err
				}
			}
			lineStart = chunk[len(chunk)-1] == '\n'
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if current != nil {
				current.Close()
			}
			return err
		}
	}
	return flush()
}

// spool holds one message: in memory, or in a temporary file past a size, as
// a message with big attachments may be.
type spool struct {
	dir  string
	buf  bytes.Buffer
	file *os.File
	size int64
}

const spoolMemory = 16 << 20

func (s *spool) Write(p []byte) (int, error) {
	s.size += int64(len(p))
	if s.file == nil && s.buf.Len()+len(p) > spoolMemory {
		f, err := os.CreateTemp(s.dir, "correo-*")
		if err != nil {
			return 0, err
		}
		s.file = f
		if _, err := f.Write(s.buf.Bytes()); err != nil {
			return 0, err
		}
		s.buf = bytes.Buffer{}
	}
	if s.file != nil {
		return s.file.Write(p)
	}
	return s.buf.Write(p)
}

func (s *spool) Open() (io.ReadCloser, error) {
	if s.file != nil {
		return os.Open(s.file.Name())
	}
	return io.NopCloser(bytes.NewReader(s.buf.Bytes())), nil
}

func (s *spool) Close() {
	if s.file != nil {
		s.file.Close()
		os.Remove(s.file.Name())
	}
}

// contentID is the hash of the whole message, for the rare one without
// Message-ID.
func (s *spool) contentID() (string, error) {
	r, err := s.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fromLineLayout is the date of the «From » line Gmail writes:
// «From 1234567890@xxx Mon Jan 02 15:04:05 +0000 2006».
const fromLineLayout = "Mon Jan 02 15:04:05 -0700 2006"

func (x *importer) message(ctx context.Context, m *spool, from string) error {
	r, err := m.Open()
	if err != nil {
		return err
	}
	parsed, parseErr := mail.ReadMessage(bufio.NewReader(r))
	r.Close()
	var header mail.Header
	if parseErr == nil {
		header = parsed.Header
	} else {
		header = mail.Header{}
	}

	id := strings.TrimSpace(header.Get("Message-Id"))
	key := ""
	if id != "" {
		key = hash16("message-id:" + id)
	} else {
		sum, err := m.contentID()
		if err != nil {
			return err
		}
		key = hash16("content:" + sum)
	}
	if x.c.Mail[key] {
		return nil
	}

	date, err := header.Date()
	if err != nil {
		if _, rest, ok := strings.Cut(from, " "); ok {
			date, err = time.Parse(fromLineLayout, strings.TrimSpace(rest))
		}
	}
	dir := "sin-fecha"
	name := key[:10] + ".eml"
	if err == nil && !date.IsZero() {
		date = date.UTC()
		dir = fmt.Sprintf("%04d/%s", date.Year(), date.Format("2006-01"))
		name = date.Format("2006-01-02_150405") + "-" + name
	} else {
		date = time.Time{}
	}
	label := pickLabel(gmailLabels(header.Get("X-Gmail-Labels")))
	out := "Correo/" + cleanPath(label) + "/" + dir + "/" + name
	// Not mutable: the same message in a later export differs at most in its
	// labels header, and the first copy is as good.
	result, err := x.run.Out.Write(ctx, plugin.File{Path: out, ModTime: date, Size: m.size, Open: m.Open})
	if err != nil {
		return err
	}
	x.created("correos", result)
	x.c.Mail[key] = true
	return nil
}

// gmailLabels splits «Recibidos,Importante,"Viajes, 2024"», decoding the
// labels with accents, which come as =?UTF-8?Q?…?=.
func gmailLabels(raw string) []string {
	if decoded, err := new(mime.WordDecoder).DecodeHeader(raw); err == nil {
		raw = decoded
	}
	labels := []string{}
	var b strings.Builder
	quoted := false
	for _, r := range raw + "," {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			if label := strings.TrimSpace(b.String()); label != "" {
				labels = append(labels, label)
			}
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	return labels
}

// Gmail labels that say something about a message but are not a place for it.
var markerLabels = map[string]bool{
	"opened": true, "abierto": true, "abiertos": true, "unread": true, "no leídos": true, "no leido": true, "no leidos": true,
	"important": true, "importante": true,
}

// systemLabels in order of preference, when the message has no label of the
// person's own.
var systemLabels = [][]string{
	{"sent", "enviados", "sent mail", "correo enviado"},
	{"inbox", "recibidos", "bandeja de entrada"},
	{"starred", "destacados"},
	{"drafts", "borradores"},
	{"chat", "chats"},
	{"archived", "archivado", "archivados"},
	{"spam"},
	{"trash", "papelera"},
}

func isSystem(label string) bool {
	for _, group := range systemLabels {
		for _, name := range group {
			if label == name {
				return true
			}
		}
	}
	return false
}

// pickLabel chooses the folder of a message: the person's own label first,
// because it changes less than «Recibidos» (a message leaves the inbox when
// it is archived), then the system ones.
func pickLabel(labels []string) string {
	useful := []string{}
	for _, label := range labels {
		lower := strings.ToLower(label)
		if markerLabels[lower] || strings.HasPrefix(lower, "category") || strings.HasPrefix(lower, "categoría") || strings.HasPrefix(lower, "categoria") {
			continue
		}
		useful = append(useful, label)
	}
	for _, label := range useful {
		if !isSystem(strings.ToLower(label)) {
			return label
		}
	}
	for _, group := range systemLabels {
		for _, label := range useful {
			for _, name := range group {
				if strings.ToLower(label) == name {
					return label
				}
			}
		}
	}
	return "Todos"
}
