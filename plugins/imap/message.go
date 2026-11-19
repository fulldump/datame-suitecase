package imap

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Entry is a message as listed in the index of its month.
type Entry struct {
	Date        string   `json:"date"` // RFC 3339, with the zone of the Date header
	From        string   `json:"from"`
	To          string   `json:"to,omitempty"`
	Subject     string   `json:"subject"`
	File        string   `json:"file"` // beside the index
	MessageID   string   `json:"message_id,omitempty"`
	Size        int      `json:"size"`
	Attachments []string `json:"attachments,omitempty"`
	Labels      []string `json:"labels,omitempty"` // Gmail
}

// Month is the index of one month of one folder, in indice.json.
type Month struct {
	Folder   string  `json:"folder"`
	Month    string  `json:"month"`
	Messages []Entry `json:"messages"`
}

// message is what is learnt from the raw bytes, which are never changed.
type message struct {
	raw         []byte
	date        time.Time
	from, to    string
	subject     string
	messageID   string
	attachments []attachment
}

type attachment struct {
	name string
	data []byte
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// decodeHeader turns «=?UTF-8?B?…?=» into text. When a charset is unknown the
// header stays as it came: the .eml beside it has the real thing anyway.
func decodeHeader(s string) string {
	out, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		out = s
	}
	if !utf8.ValidString(out) {
		out = strings.ToValidUTF8(out, "?")
	}
	return strings.Join(strings.Fields(out), " ")
}

// parse reads what the index and the file name need. A broken message is
// still saved: what cannot be read is just left empty.
func parse(raw []byte, internal time.Time, extract bool) message {
	m := message{raw: raw, date: internal}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return m
	}
	h := msg.Header
	if d, err := mail.ParseDate(h.Get("Date")); err == nil {
		m.date = d
	}
	m.subject = decodeHeader(h.Get("Subject"))
	m.from = decodeHeader(h.Get("From"))
	m.to = decodeHeader(h.Get("To"))
	m.messageID = strings.TrimSpace(h.Get("Message-Id"))
	walk(textproto.MIMEHeader(h), msg.Body, 0, func(name string, data []byte) {
		if !extract {
			data = nil // only the names are kept for the index
		}
		m.attachments = append(m.attachments, attachment{name: name, data: data})
	})
	return m
}

// walk goes through the MIME parts and calls found for each attachment.
func walk(h textproto.MIMEHeader, body io.Reader, depth int, found func(string, []byte)) {
	if depth > 10 {
		return
	}
	mediaType, params, _ := mime.ParseMediaType(h.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
		reader := multipart.NewReader(body, params["boundary"])
		for {
			// NextRawPart: the transfer encoding is decoded here, the same
			// way for every part.
			part, err := reader.NextRawPart()
			if err != nil {
				return
			}
			walk(part.Header, part, depth+1, found)
		}
	}
	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	if name == "" && disposition != "attachment" {
		return
	}
	if name == "" {
		name = "adjunto"
		if mediaType == "message/rfc822" {
			name += ".eml"
		}
	}
	data, err := io.ReadAll(decodeTransfer(h.Get("Content-Transfer-Encoding"), io.LimitReader(body, maxLiteral)))
	if err != nil {
		return
	}
	found(decodeHeader(name), data)
}

func decodeTransfer(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &skipSpaces{r: r})
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// skipSpaces drops the line breaks of base64 bodies, which the decoder of
// the standard library does not accept in the middle.
type skipSpaces struct{ r io.Reader }

func (s *skipSpaces) Read(p []byte) (int, error) {
	for {
		n, err := s.r.Read(p)
		k := 0
		for _, b := range p[:n] {
			if b != '\r' && b != '\n' && b != ' ' && b != '\t' {
				p[k] = b
				k++
			}
		}
		if k > 0 || err != nil {
			return k, err
		}
	}
}

// charsetReader knows the old Western charsets that the standard library does
// not (it knows UTF-8, ISO-8859-1 and US-ASCII).
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "windows-1252", "cp1252", "iso-8859-15", "latin-9", "latin9":
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		for _, c := range data {
			b.WriteRune(cp1252(c, strings.Contains(strings.ToLower(charset), "8859")))
		}
		return strings.NewReader(b.String()), nil
	}
	return nil, errors.New("charset desconocido: " + charset)
}

var cp1252High = []rune("€\u0081‚ƒ„…†‡ˆ‰Š‹Œ\u008dŽ\u008f\u0090‘’“”•–—˜™š›œ\u009džŸ")

func cp1252(c byte, latin9 bool) rune {
	if latin9 {
		switch c {
		case 0xA4:
			return '€'
		case 0xA6:
			return 'Š'
		case 0xA8:
			return 'š'
		case 0xB4:
			return 'Ž'
		case 0xB8:
			return 'ž'
		case 0xBC:
			return 'Œ'
		case 0xBD:
			return 'œ'
		case 0xBE:
			return 'Ÿ'
		}
		return rune(c)
	}
	if c >= 0x80 && c < 0xA0 {
		return cp1252High[c-0x80]
	}
	return rune(c)
}

// contentHash names a message by its bytes, which IMAP never changes: the
// same message gets the same name in every run, even when the server
// renumbers its UIDs, so it cannot be saved twice.
func contentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:5])
}

// fileName is «2024-03-01-factura-de-marzo-1a2b3c4d5e.eml».
func (m message) fileName() string {
	slug := plugin.Slug(m.subject, 50)
	if slug == "" {
		slug = "sin-asunto"
	}
	return fmt.Sprintf("%s-%s-%s.eml", m.date.Format("2006-01-02"), slug, contentHash(m.raw))
}

// monthDir is «2024/2024-03».
func (m message) monthDir() string {
	return m.date.Format("2006") + "/" + m.date.Format("2006-01")
}

func (m message) entry(file string, labels []string) Entry {
	e := Entry{Date: m.date.Format(time.RFC3339), From: m.from, To: m.to, Subject: m.subject, File: file,
		MessageID: m.messageID, Size: len(m.raw), Labels: labels}
	for _, a := range m.attachments {
		e.Attachments = append(e.Attachments, a.name)
	}
	return e
}

// attachmentNames gives each attachment a safe and unique name in its folder.
func attachmentNames(list []attachment) []string {
	used := map[string]bool{}
	out := make([]string, len(list))
	for i, a := range list {
		name := plugin.SafeName(a.name, 120)
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%d-%s", n, plugin.SafeName(a.name, 110))
		}
		used[strings.ToLower(name)] = true
		out[i] = name
	}
	return out
}

// merge adds entries to a month, by file, and keeps it in date order.
func (mo *Month) merge(entries []Entry) {
	byFile := map[string]int{}
	for i, e := range mo.Messages {
		byFile[e.File] = i
	}
	for _, e := range entries {
		if i, ok := byFile[e.File]; ok {
			mo.Messages[i] = e
			continue
		}
		byFile[e.File] = len(mo.Messages)
		mo.Messages = append(mo.Messages, e)
	}
	sort.SliceStable(mo.Messages, func(i, j int) bool {
		if mo.Messages[i].Date != mo.Messages[j].Date {
			return mo.Messages[i].Date < mo.Messages[j].Date
		}
		return mo.Messages[i].File < mo.Messages[j].File
	})
}

var monthNames = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

// markdown is the index for people: a table with a link to each message.
func (mo *Month) markdown() []byte {
	var b bytes.Buffer
	title := mo.Month
	if t, err := time.Parse("2006-01", mo.Month); err == nil {
		title = monthNames[t.Month()-1] + " de " + t.Format("2006")
	}
	fmt.Fprintf(&b, "# %s: %s\n\n", mo.Folder, title)
	fmt.Fprintf(&b, "%d correos. Cada uno es un fichero `.eml`, que abre cualquier programa de correo (Thunderbird, Outlook, Correo de Apple) y que es texto que se puede leer tal cual.\n\n", len(mo.Messages))
	b.WriteString("| Fecha | De | Asunto |\n|---|---|---|\n")
	for _, e := range mo.Messages {
		date := e.Date
		if t, err := time.Parse(time.RFC3339, e.Date); err == nil {
			date = t.Format("2006-01-02 15:04")
		}
		subject := e.Subject
		if subject == "" {
			subject = "(sin asunto)"
		}
		link := fmt.Sprintf("[%s](%s)", cell(subject), e.File)
		if n := len(e.Attachments); n == 1 {
			link += " (1 adjunto)"
		} else if n > 1 {
			link += fmt.Sprintf(" (%d adjuntos)", n)
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", date, cell(e.From), link)
	}
	return b.Bytes()
}

// cell keeps a text from breaking the table or the link around it.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "[", `\[`, "]", `\]`, "<", "&lt;", ">", "&gt;").Replace(s)
}
