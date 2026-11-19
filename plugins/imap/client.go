package imap

// A minimal IMAP4rev1 client (RFC 3501), with only what a read-only copy
// needs: LOGIN, LIST, EXAMINE, UID SEARCH and UID FETCH with BODY.PEEK.
//
// It is written here instead of taking a library because the subset is small
// and stable (IMAP has not changed in decades), it keeps the program free of
// dependencies, and it makes it easy to be sure that nothing ever modifies the
// mailbox: there is no code for SELECT, STORE, EXPUNGE or APPEND at all.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	// commandTimeout bounds each command, so a server that stops answering
	// does not hang the copy for ever. A batch of big messages over a slow
	// line is what takes longest.
	commandTimeout = 5 * time.Minute
	// maxLiteral is the biggest message read into memory. Providers cap
	// messages at 25-50 MB; anything bigger is skipped with a note in the log.
	maxLiteral = 200 << 20
)

// atom is a bare word of the protocol (OK, FETCH, \Seen), as opposed to a
// quoted string or a literal, which are plain strings and []byte.
type atom string

// tooBig stands for a literal that was read and thrown away.
type tooBig int64

// response is one line (with its literals) from the server.
type response struct {
	tag    string // "*" untagged, "+" continuation, or the tag of a command
	kind   string // upper case: OK, NO, LIST, FETCH, EXISTS, SEARCH…
	num    uint32 // the number of «* 12 FETCH» or «* 3 EXISTS»
	fields []any  // the data of LIST, SEARCH, FETCH…
	text   string // the rest of the line of OK, NO, BAD, BYE
}

// errAuth is a rejected login; the message is the server's.
type errAuth struct{ text string }

func (e *errAuth) Error() string { return e.text }

// errNo is a command the server refused.
type errNo struct{ text string }

func (e *errNo) Error() string { return e.text }

type client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	tag  int
	caps map[string]bool
	host string
}

// dial connects with TLS from the first byte (port 993) or upgrades a plain
// connection with STARTTLS (port 143). There is no way to talk without TLS:
// the password would travel in clear.
func dial(ctx context.Context, host string, port int, starttls bool, roots *x509.CertPool) (*client, error) {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 30 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar con %s: %w. Comprueba el servidor y el puerto", address, err)
	}
	config := &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}
	c := &client{host: host}
	if !starttls {
		if err := c.handshake(ctx, raw, config); err != nil {
			return nil, err
		}
		if err := c.greeting(); err != nil {
			c.conn.Close()
			return nil, err
		}
		return c, nil
	}
	c.setConn(raw)
	if err := c.greeting(); err != nil {
		raw.Close()
		return nil, err
	}
	if err := c.capability(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	if !c.caps["STARTTLS"] {
		raw.Close()
		return nil, fmt.Errorf("%s no ofrece STARTTLS, así que la contraseña iría sin cifrar: desmarca STARTTLS y usa el puerto 993", host)
	}
	if _, err := c.cmd(ctx, nil, "STARTTLS"); err != nil {
		raw.Close()
		return nil, fmt.Errorf("%s no acepta STARTTLS: %w", host, err)
	}
	if err := c.handshake(ctx, raw, config); err != nil {
		return nil, err
	}
	c.caps = nil // what was said in clear must not be trusted (RFC 3501 6.2.1)
	return c, nil
}

func (c *client) handshake(ctx context.Context, raw net.Conn, config *tls.Config) error {
	conn := tls.Client(raw, config)
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return fmt.Errorf("no se pudo establecer la conexión segura con %s: %w", c.host, err)
	}
	c.setConn(conn)
	return nil
}

func (c *client) setConn(conn net.Conn) {
	c.conn = conn
	c.r = bufio.NewReaderSize(conn, 64<<10)
	c.w = bufio.NewWriter(conn)
}

func (c *client) greeting() error {
	c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	resp, err := c.read()
	if err != nil {
		return fmt.Errorf("%s no responde como un servidor IMAP: %w", c.host, err)
	}
	if resp.tag != "*" || (resp.kind != "OK" && resp.kind != "PREAUTH") {
		return fmt.Errorf("%s no acepta la conexión: %s", c.host, resp.text)
	}
	return nil
}

// Close says goodbye politely, without waiting long.
func (c *client) Close() {
	if c.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.cmd(ctx, nil, "LOGOUT")
	c.conn.Close()
}

// literal is an argument sent as {n} followed by the bytes, for values that a
// quoted string cannot carry (8-bit characters in a password).
type literal string

// quote returns an argument as a quoted string, or as a literal when needed.
func quote(s string) any {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || s[i] == '\r' || s[i] == '\n' || s[i] == 0 {
			return literal(s)
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// cmd sends a command and reads until its tagged answer, giving each
// untagged response to fn. It returns the text of the final OK.
func (c *client) cmd(ctx context.Context, fn func(*response) error, parts ...any) (string, error) {
	// A cancelled context breaks any read or write at once.
	stop := context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	c.conn.SetDeadline(time.Now().Add(commandTimeout))
	text, err := c.exchange(fn, parts...)
	if err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	return text, err
}

func (c *client) exchange(fn func(*response) error, parts ...any) (string, error) {
	c.tag++
	tag := fmt.Sprintf("A%04d", c.tag)
	line := tag
	for _, part := range parts {
		switch p := part.(type) {
		case literal:
			fmt.Fprintf(c.w, "%s {%d}\r\n", line, len(p))
			if err := c.w.Flush(); err != nil {
				return "", err
			}
			if err := c.waitContinuation(tag); err != nil {
				return "", err
			}
			line = string(p)
			continue
		case string:
			line += " " + p
		default:
			line += " " + fmt.Sprint(p)
		}
	}
	c.w.WriteString(line + "\r\n")
	if err := c.w.Flush(); err != nil {
		return "", err
	}
	for {
		resp, err := c.read()
		if err != nil {
			return "", err
		}
		switch resp.tag {
		case tag:
			switch resp.kind {
			case "OK":
				return resp.text, nil
			case "NO", "BAD":
				return "", &errNo{resp.text}
			default:
				return "", fmt.Errorf("respuesta inesperada: %s %s", resp.kind, resp.text)
			}
		case "*":
			if resp.kind == "BYE" {
				// Logging out gets a BYE before its OK; anything else is the
				// server hanging up, and the next read will fail.
				continue
			}
			if fn != nil {
				if err := fn(resp); err != nil {
					return "", err
				}
			}
		}
	}
}

func (c *client) waitContinuation(tag string) error {
	for {
		resp, err := c.read()
		if err != nil {
			return err
		}
		if resp.tag == "+" {
			return nil
		}
		if resp.tag == tag {
			return &errNo{resp.text}
		}
	}
}

func (c *client) capability(ctx context.Context) error {
	c.caps = map[string]bool{}
	_, err := c.cmd(ctx, func(r *response) error {
		if r.kind == "CAPABILITY" {
			for _, f := range r.fields {
				c.caps[strings.ToUpper(str(f))] = true
			}
		}
		return nil
	}, "CAPABILITY")
	return err
}

func (c *client) login(ctx context.Context, user, password string) error {
	if _, err := c.cmd(ctx, nil, "LOGIN", quote(user), quote(password)); err != nil {
		var no *errNo
		if errors.As(err, &no) {
			return &errAuth{no.text}
		}
		return err
	}
	// Capabilities may change once logged in (Gmail adds X-GM-EXT-1).
	return c.capability(ctx)
}

// mailbox is a folder as LIST describes it.
type mailbox struct {
	Name      string // as the server names it (modified UTF-7), for commands
	Display   string // decoded, for people
	Delimiter string
	Attrs     map[string]bool // lower case: \noselect, \all, \trash, \junk…
}

func (c *client) list(ctx context.Context) ([]mailbox, error) {
	var out []mailbox
	_, err := c.cmd(ctx, func(r *response) error {
		if r.kind != "LIST" || len(r.fields) < 3 {
			return nil
		}
		m := mailbox{Attrs: map[string]bool{}, Name: str(r.fields[2]), Delimiter: str(r.fields[1])}
		if attrs, ok := r.fields[0].([]any); ok {
			for _, a := range attrs {
				m.Attrs[strings.ToLower(str(a))] = true
			}
		}
		if strings.EqualFold(m.Name, "INBOX") {
			m.Name = "INBOX"
		}
		m.Display = decodeUTF7(m.Name)
		out = append(out, m)
		return nil
	}, "LIST", `""`, `"*"`)
	return out, err
}

// status is what EXAMINE says about a folder.
type status struct {
	Exists      uint32
	UIDValidity uint32
	UIDNext     uint32
}

// examine opens a folder read only: unlike SELECT, the server must not change
// anything in it, not even the \Recent flags.
func (c *client) examine(ctx context.Context, name string) (status, error) {
	var s status
	_, err := c.cmd(ctx, func(r *response) error {
		switch r.kind {
		case "EXISTS":
			s.Exists = r.num
		case "OK":
			if v, ok := responseCode(r.text, "UIDVALIDITY"); ok {
				s.UIDValidity = v
			}
			if v, ok := responseCode(r.text, "UIDNEXT"); ok {
				s.UIDNext = v
			}
		}
		return nil
	}, "EXAMINE", quote(name))
	return s, err
}

func responseCode(text, code string) (uint32, bool) {
	prefix := "[" + code + " "
	if !strings.HasPrefix(strings.ToUpper(text), prefix) {
		return 0, false
	}
	rest := text[len(prefix):]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return 0, false
	}
	v, err := strconv.ParseUint(rest[:end], 10, 32)
	return uint32(v), err == nil
}

// uidsAfter returns the UIDs greater than last, in order.
func (c *client) uidsAfter(ctx context.Context, last uint32) ([]uint32, error) {
	var out []uint32
	_, err := c.cmd(ctx, func(r *response) error {
		if r.kind != "SEARCH" {
			return nil
		}
		for _, f := range r.fields {
			v, err := strconv.ParseUint(str(f), 10, 32)
			// «n:*» includes the highest UID even when it is below n.
			if err == nil && uint32(v) > last {
				out = append(out, uint32(v))
			}
		}
		return nil
	}, "UID SEARCH UID", fmt.Sprintf("%d:*", last+1))
	slices.Sort(out)
	return out, err
}

// fetched is one message of a FETCH answer.
type fetched struct {
	UID          uint32
	Size         int64
	InternalDate time.Time
	Body         []byte // the whole message, or only the headers asked
	TooBig       bool
	Labels       []string // Gmail labels
}

// fetch asks for some messages and gives each one to fn as it arrives, so
// only one message is in memory at a time. items is the FETCH list; it must
// use BODY.PEEK, which does not mark anything as read.
func (c *client) fetch(ctx context.Context, uids []uint32, items string, fn func(fetched) error) error {
	set := make([]string, len(uids))
	for i, uid := range uids {
		set[i] = strconv.FormatUint(uint64(uid), 10)
	}
	_, err := c.cmd(ctx, func(r *response) error {
		if r.kind != "FETCH" || len(r.fields) == 0 {
			return nil
		}
		pairs, ok := r.fields[0].([]any)
		if !ok {
			return nil
		}
		var m fetched
		hasBody := false
		for i := 0; i+1 < len(pairs); i += 2 {
			key := strings.ToUpper(str(pairs[i]))
			value := pairs[i+1]
			switch {
			case key == "UID":
				v, _ := strconv.ParseUint(str(value), 10, 32)
				m.UID = uint32(v)
			case key == "RFC822.SIZE":
				m.Size, _ = strconv.ParseInt(str(value), 10, 64)
			case key == "INTERNALDATE":
				m.InternalDate, _ = time.Parse("_2-Jan-2006 15:04:05 -0700", str(value))
			case key == "X-GM-LABELS":
				list, _ := value.([]any)
				for _, l := range list {
					m.Labels = append(m.Labels, decodeUTF7(str(l)))
				}
			case strings.HasPrefix(key, "BODY["):
				hasBody = true
				switch v := value.(type) {
				case []byte:
					m.Body = v
				case string:
					m.Body = []byte(v)
				case tooBig:
					m.TooBig = true
					m.Size = int64(v)
				}
			}
		}
		// Servers also send FETCH on their own, for flag changes: those have
		// no body and are not ours.
		if m.UID == 0 || !hasBody {
			return nil
		}
		return fn(m)
	}, "UID FETCH", strings.Join(set, ","), items)
	return err
}

// ---- Reading responses ----

func (c *client) read() (*response, error) {
	tag, err := c.atom()
	if err != nil {
		return nil, err
	}
	r := &response{tag: tag}
	if tag == "+" {
		r.text, err = c.restOfLine()
		return r, err
	}
	if err := c.space(); err != nil {
		return nil, err
	}
	if tag != "*" {
		kind, err := c.atom()
		if err != nil {
			return nil, err
		}
		r.kind = strings.ToUpper(kind)
		r.text, err = c.restOfLine()
		return r, err
	}
	first, err := c.atom()
	if err != nil {
		return nil, err
	}
	if n, err := strconv.ParseUint(first, 10, 32); err == nil {
		r.num = uint32(n)
		c.space()
		kind, err := c.atom()
		if err != nil {
			return nil, err
		}
		r.kind = strings.ToUpper(kind)
		if r.kind == "FETCH" {
			c.space()
			item, err := c.item()
			if err != nil {
				return nil, err
			}
			r.fields = []any{item}
		}
		_, err = c.restOfLine()
		return r, err
	}
	r.kind = strings.ToUpper(first)
	switch r.kind {
	case "OK", "NO", "BAD", "BYE", "PREAUTH":
		r.text, err = c.restOfLine()
		return r, err
	}
	for {
		b, err := c.skipSpaces()
		if err != nil {
			return nil, err
		}
		if b == '\r' || b == '\n' {
			_, err = c.restOfLine()
			return r, err
		}
		item, err := c.item()
		if err != nil {
			return nil, err
		}
		r.fields = append(r.fields, item)
	}
}

func (c *client) space() error {
	b, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	if b != ' ' {
		c.r.UnreadByte()
	}
	return nil
}

func (c *client) skipSpaces() (byte, error) {
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != ' ' {
			c.r.UnreadByte()
			return b, nil
		}
	}
}

func (c *client) restOfLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimLeft(strings.TrimRight(line, "\r\n"), " "), nil
}

// atom reads a bare word. A part in brackets is kept whole, spaces included,
// because FETCH answers name items like «BODY[HEADER.FIELDS (MESSAGE-ID)]».
func (c *client) atom() (string, error) {
	var b strings.Builder
	for {
		ch, err := c.r.ReadByte()
		if err != nil {
			return "", err
		}
		switch ch {
		case ' ', '(', ')', '\r', '\n':
			c.r.UnreadByte()
			return b.String(), nil
		case '[':
			b.WriteByte(ch)
			inner, err := c.r.ReadString(']')
			if err != nil {
				return "", err
			}
			b.WriteString(inner)
		default:
			b.WriteByte(ch)
		}
		if b.Len() > 64<<10 {
			return "", errors.New("respuesta del servidor demasiado larga")
		}
	}
}

func (c *client) item() (any, error) {
	b, err := c.r.ReadByte()
	if err != nil {
		return nil, err
	}
	switch b {
	case '(':
		var list []any
		for {
			next, err := c.skipSpaces()
			if err != nil {
				return nil, err
			}
			if next == ')' {
				c.r.ReadByte()
				return list, nil
			}
			if next == '\r' || next == '\n' {
				return nil, errors.New("lista sin cerrar en la respuesta del servidor")
			}
			item, err := c.item()
			if err != nil {
				return nil, err
			}
			list = append(list, item)
		}
	case '"':
		var s strings.Builder
		for {
			ch, err := c.r.ReadByte()
			if err != nil {
				return nil, err
			}
			switch ch {
			case '"':
				return s.String(), nil
			case '\\':
				ch, err = c.r.ReadByte()
				if err != nil {
					return nil, err
				}
			case '\r', '\n':
				return nil, errors.New("texto sin cerrar en la respuesta del servidor")
			}
			s.WriteByte(ch)
		}
	case '{':
		size, err := c.r.ReadString('}')
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSuffix(size, "}"), "+"), 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("literal no válido: {%s", size)
		}
		if _, err := c.restOfLine(); err != nil {
			return nil, err
		}
		if n > maxLiteral {
			if _, err := io.CopyN(io.Discard, c.r, n); err != nil {
				return nil, err
			}
			return tooBig(n), nil
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c.r, data); err != nil {
			return nil, err
		}
		return data, nil
	}
	c.r.UnreadByte()
	word, err := c.atom()
	if err != nil {
		return nil, err
	}
	if word == "" {
		// Not an item at all: consume the odd byte so parsing moves on.
		ch, _ := c.r.ReadByte()
		return atom(ch), nil
	}
	if strings.EqualFold(word, "NIL") {
		return nil, nil
	}
	return atom(word), nil
}

// str turns any simple item into text.
func str(v any) string {
	switch s := v.(type) {
	case atom:
		return string(s)
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}

// decodeUTF7 decodes the modified UTF-7 of folder names (RFC 3501 5.1.3):
// «Caf&AOk-» is «Café». Text that is not valid is returned as it came.
func decodeUTF7(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			out.WriteByte(s[i])
			continue
		}
		end := strings.IndexByte(s[i:], '-')
		if end < 0 {
			return s
		}
		chunk := s[i+1 : i+end]
		i += end
		if chunk == "" {
			out.WriteByte('&')
			continue
		}
		units, ok := decodeBase64UTF16(chunk)
		if !ok {
			return s
		}
		out.WriteString(string(utf16.Decode(units)))
	}
	return out.String()
}

const utf7Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,"

func decodeBase64UTF16(chunk string) ([]uint16, bool) {
	var bits, nbits uint32
	var bytes []byte
	for i := 0; i < len(chunk); i++ {
		v := strings.IndexByte(utf7Alphabet, chunk[i])
		if v < 0 {
			return nil, false
		}
		bits = bits<<6 | uint32(v)
		nbits += 6
		if nbits >= 8 {
			nbits -= 8
			bytes = append(bytes, byte(bits>>nbits))
		}
	}
	if len(bytes)%2 != 0 {
		return nil, false
	}
	units := make([]uint16, len(bytes)/2)
	for i := range units {
		units[i] = uint16(bytes[2*i])<<8 | uint16(bytes[2*i+1])
	}
	return units, true
}
