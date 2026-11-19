package imap_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/imap"
	"github.com/fulldump/datame-suitcase/plugintest"
)

// ---- A fake IMAP server ----

type fakeMessage struct {
	uid    uint32
	raw    string
	labels []string
}

type fakeBox struct {
	name     string // as on the wire (modified UTF-7)
	attrs    string
	validity uint32
	msgs     []fakeMessage
}

type fakeIMAP struct {
	listener net.Listener
	tls      *tls.Config

	mutex      sync.Mutex
	user, pass string
	gmail      bool
	starttls   bool // plain listener that offers STARTTLS
	noSTARTTLS bool // plain listener that does not offer it
	boxes      []*fakeBox
	commands   []string
	bodies     int // full messages sent
	dropAfter  int // hang up when this many bodies were sent (0: never)
}

// newFake starts a server on 127.0.0.1 with its own certificate, and returns
// an HTTP client that trusts it (the integration takes its CAs from there).
func newFake(t *testing.T, plain bool) (*fakeIMAP, *http.Client) {
	cert, pool := certificate(t)
	f := &fakeIMAP{user: "ana@example.org", pass: "abcd efgh ijkl mnop", tls: &tls.Config{Certificates: []tls.Certificate{cert}}}
	var err error
	if plain {
		f.listener, err = net.Listen("tcp", "127.0.0.1:0")
	} else {
		f.listener, err = tls.Listen("tcp", "127.0.0.1:0", f.tls)
	}
	biff.AssertNil(err)
	t.Cleanup(func() { f.listener.Close() })
	go func() {
		for {
			conn, err := f.listener.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, !plain)
		}
	}()
	return f, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

func (f *fakeIMAP) port() string {
	return strconv.Itoa(f.listener.Addr().(*net.TCPAddr).Port)
}

func (f *fakeIMAP) box(name string) *fakeBox {
	for _, b := range f.boxes {
		if b.name == name {
			return b
		}
	}
	return nil
}

func (f *fakeIMAP) log() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeIMAP) serve(conn net.Conn, secure bool) {
	defer func() { conn.Close() }()
	r := bufio.NewReader(conn)
	send := func(format string, args ...any) { fmt.Fprintf(conn, format, args...) }
	send("* OK fake IMAP ready\r\n")
	authed := false
	var selected *fakeBox
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		tag, rest, _ := strings.Cut(line, " ")
		word, args, _ := strings.Cut(rest, " ")
		word = strings.ToUpper(word)
		if word == "UID" {
			var sub string
			sub, args, _ = strings.Cut(args, " ")
			word += " " + strings.ToUpper(sub)
		}
		f.mutex.Lock()
		if word == "LOGIN" {
			f.commands = append(f.commands, "LOGIN") // not the password
		} else {
			f.commands = append(f.commands, rest)
		}
		f.mutex.Unlock()

		switch word {
		case "CAPABILITY":
			caps := "IMAP4rev1"
			if !secure && f.starttls {
				caps += " STARTTLS"
			}
			if authed && f.gmail {
				caps += " X-GM-EXT-1"
			}
			send("* CAPABILITY %s\r\n%s OK done\r\n", caps, tag)
		case "STARTTLS":
			if secure || !f.starttls {
				send("%s BAD no\r\n", tag)
				continue
			}
			send("%s OK go ahead\r\n", tag)
			tconn := tls.Server(conn, f.tls)
			if tconn.Handshake() != nil {
				return
			}
			conn, r, secure = tconn, bufio.NewReader(tconn), true
		case "LOGIN":
			if !secure {
				send("%s NO [PRIVACYREQUIRED] use TLS\r\n", tag)
				continue
			}
			parts := quoted(args)
			f.mutex.Lock()
			ok := len(parts) == 2 && parts[0] == f.user && parts[1] == f.pass
			f.mutex.Unlock()
			if !ok {
				send("%s NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)\r\n", tag)
				continue
			}
			authed = true
			send("%s OK logged in\r\n", tag)
		case "LIST":
			f.mutex.Lock()
			for _, b := range f.boxes {
				send("* LIST (%s) \"/\" \"%s\"\r\n", b.attrs, b.name)
			}
			f.mutex.Unlock()
			send("%s OK done\r\n", tag)
		case "EXAMINE", "SELECT":
			f.mutex.Lock()
			names := quoted(args)
			selected = nil
			if len(names) == 1 {
				selected = f.box(names[0])
			}
			if selected == nil {
				f.mutex.Unlock()
				send("%s NO no such mailbox\r\n", tag)
				continue
			}
			next := uint32(1)
			for _, m := range selected.msgs {
				next = max(next, m.uid+1)
			}
			send("* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n* %d EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY %d] UIDs valid\r\n* OK [UIDNEXT %d] Predicted next UID\r\n%s OK [READ-ONLY] done\r\n",
				len(selected.msgs), selected.validity, next, tag)
			f.mutex.Unlock()
		case "UID SEARCH":
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(strings.ToUpper(args), "UID "), ":*"))
			f.mutex.Lock()
			var out []string
			var highest uint32
			for _, m := range selected.msgs {
				highest = max(highest, m.uid)
				if m.uid >= uint32(from) {
					out = append(out, strconv.Itoa(int(m.uid)))
				}
			}
			if len(out) == 0 && highest > 0 {
				out = append(out, strconv.Itoa(int(highest))) // as real servers do with n:*
			}
			f.mutex.Unlock()
			send("* SEARCH %s\r\n%s OK done\r\n", strings.Join(out, " "), tag)
		case "UID FETCH":
			set, items, _ := strings.Cut(args, " ")
			want := map[uint32]bool{}
			for _, s := range strings.Split(set, ",") {
				n, _ := strconv.Atoi(s)
				want[uint32(n)] = true
			}
			// A flag change the client did not ask for, as servers send.
			send("* 1 FETCH (FLAGS (\\Seen))\r\n")
			f.mutex.Lock()
			msgs := append([]fakeMessage(nil), selected.msgs...)
			f.mutex.Unlock()
			// In reverse order: servers may answer in any order.
			for i := len(msgs) - 1; i >= 0; i-- {
				m := msgs[i]
				if !want[m.uid] {
					continue
				}
				if strings.Contains(items, "HEADER.FIELDS") {
					header := ""
					for _, l := range strings.Split(m.raw, "\r\n") {
						if strings.HasPrefix(strings.ToLower(l), "message-id:") {
							header = l + "\r\n"
						}
					}
					header += "\r\n"
					send("* %d FETCH (UID %d RFC822.SIZE %d BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)\r\n", i+1, m.uid, len(m.raw), len(header), header)
					continue
				}
				f.mutex.Lock()
				if f.dropAfter > 0 && f.bodies >= f.dropAfter {
					f.mutex.Unlock()
					return // the connection is cut
				}
				f.bodies++
				f.mutex.Unlock()
				labels := ""
				if strings.Contains(items, "X-GM-LABELS") {
					labels = " X-GM-LABELS (" + strings.Join(m.labels, " ") + ")"
				}
				send("* %d FETCH (BODY[] {%d}\r\n%s UID %d INTERNALDATE \"01-Jan-2020 00:00:00 +0000\" RFC822.SIZE %d%s)\r\n", i+1, len(m.raw), m.raw, m.uid, len(m.raw), labels)
			}
			send("%s OK done\r\n", tag)
		case "LOGOUT":
			send("* BYE bye\r\n%s OK done\r\n", tag)
			return
		default:
			send("%s BAD unknown\r\n", tag)
		}
	}
}

// quoted reads the quoted strings of a command line.
func quoted(s string) []string {
	var out []string
	for {
		start := strings.IndexByte(s, '"')
		if start < 0 {
			return out
		}
		var b strings.Builder
		i := start + 1
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		out = append(out, b.String())
		if i >= len(s) {
			return out
		}
		s = s[i+1:]
	}
}

func certificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	biff.AssertNil(err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake imap"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	biff.AssertNil(err)
	parsed, err := x509.ParseCertificate(der)
	biff.AssertNil(err)
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// ---- Synthetic messages ----

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

var (
	plainMessage = crlf(`From: =?UTF-8?Q?Ana_P=C3=A9rez?= <ana@example.org>
To: yo@example.org
Subject: Hola
Date: Fri, 01 Mar 2024 10:00:00 +0100
Message-ID: <m1@example.org>
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: 8bit

Hola, ¿qué tal?
`)

	attachmentMessage = crlf(`From: Facturas <facturas@example.com>
To: yo@example.org
Subject: =?UTF-8?B?` + base64.StdEncoding.EncodeToString([]byte("Factura de marzo – ñandú")) + `?=
Date: Fri, 15 Mar 2024 09:30:00 +0000
Message-ID: <m2@example.com>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="frontera"

--frontera
Content-Type: text/plain; charset=utf-8

Adjunto la factura.
--frontera
Content-Type: application/pdf; name="informe.pdf"
Content-Disposition: attachment; filename="informe.pdf"
Content-Transfer-Encoding: base64

` + base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 falso\n")) + `
--frontera
Content-Type: text/plain; charset=utf-8
Content-Disposition: attachment; filename*=UTF-8''a%C3%B1o.txt
Content-Transfer-Encoding: quoted-printable

A=C3=B1o nuevo
--frontera--
`)

	latinMessage = crlf(`From: beto@example.net
Subject: =?ISO-8859-1?Q?Caf=E9_con_le=F1a?=
Date: Tue, 02 Apr 2024 08:00:00 +0200

Sin Message-ID.
`)

	trashMessage = crlf(`From: spam@example.com
Subject: Basura
Date: Wed, 03 Apr 2024 08:00:00 +0000
Message-ID: <m4@example.com>

Nada.
`)

	euroMessage = crlf(`From: tienda@example.com
Subject: =?windows-1252?Q?Precio_=80?=
Date: Mon, 06 May 2024 12:00:00 +0000
Message-ID: <m5@example.com>

Caro.
`)
)

func eml(month, date, slug, raw string) string {
	return month + "/" + date + "-" + slug + "-" + plugin.ShortHash(raw) + ".eml"
}

func standardBoxes() []*fakeBox {
	return []*fakeBox{
		{name: "INBOX", attrs: `\HasNoChildren`, validity: 1, msgs: []fakeMessage{{uid: 1, raw: plainMessage}, {uid: 2, raw: attachmentMessage}, {uid: 5, raw: latinMessage}}},
		{name: "Trash", attrs: `\HasNoChildren \Trash`, validity: 1, msgs: []fakeMessage{{uid: 1, raw: trashMessage}}},
		{name: "Caf&AOk-", attrs: `\HasNoChildren`, validity: 7, msgs: []fakeMessage{{uid: 3, raw: euroMessage}}},
		{name: "Archivo", attrs: `\Noselect \HasChildren`, validity: 1},
		{name: "Archivo/2023", attrs: `\HasNoChildren`, validity: 1},
	}
}

func setup(t *testing.T, config plugin.Config) (*fakeIMAP, *plugintest.Harness) {
	f, client := newFake(t, false)
	f.boxes = standardBoxes()
	base := plugin.Config{"server": "127.0.0.1", "port": f.port(), "user": f.user, "password": f.pass}
	for k, v := range config {
		base[k] = v
	}
	h := plugintest.New(t, imap.Plugin{}, base)
	h.Folder = "/Archivo/Correo"
	h.HTTP = client
	return f, h
}

func bodyFetches(commands []string) []string {
	var out []string
	for _, c := range commands {
		if strings.HasPrefix(c, "UID FETCH") && strings.Contains(c, "BODY.PEEK[]") {
			out = append(out, c)
		}
	}
	return out
}

// Nothing may change the mailbox: no SELECT, no STORE, no body without PEEK.
func assertReadOnly(t *testing.T, commands []string) {
	t.Helper()
	for _, c := range commands {
		upper := strings.ToUpper(c)
		if strings.HasPrefix(upper, "SELECT") || strings.Contains(upper, "STORE") || strings.Contains(upper, "EXPUNGE") ||
			(strings.Contains(upper, "FETCH") && strings.Contains(upper, "BODY[")) {
			t.Fatalf("a command that could change the mailbox: %s", c)
		}
	}
}

// ---- Tests ----

func TestIMAPFirstRunAndRerun(t *testing.T) {
	f, h := setup(t, plugin.Config{"attachments": "true"})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 4) // the trash is skipped

	// The bytes, untouched.
	plain := eml("INBOX/2024/2024-03", "2024-03-01", "hola", plainMessage)
	biff.AssertEqual(string(h.File(plain)), plainMessage)
	invoice := eml("INBOX/2024/2024-03", "2024-03-15", "factura-de-marzo-nandu", attachmentMessage)
	biff.AssertEqual(string(h.File(invoice)), attachmentMessage)
	biff.AssertEqual(string(h.File(eml("INBOX/2024/2024-04", "2024-04-02", "cafe-con-lena", latinMessage))), latinMessage)
	biff.AssertEqual(string(h.File(eml("Cafe/2024/2024-05", "2024-05-06", "precio", euroMessage))), euroMessage)
	for _, p := range h.NAS.Paths("principal") {
		biff.AssertFalse(strings.Contains(p, "Trash"))
		biff.AssertFalse(strings.Contains(p, "Archivo/Correo/Archivo"))
	}

	// The attachments, also loose.
	folder := strings.TrimSuffix(invoice, ".eml")
	biff.AssertEqual(string(h.File(folder+"/informe.pdf")), "%PDF-1.4 falso\n")
	biff.AssertEqual(string(h.File(folder+"/ano.txt")), "Año nuevo")

	// The month index, decoded for people.
	var month imap.Month
	biff.AssertNil(json.Unmarshal(h.File("INBOX/2024/2024-03/indice.json"), &month))
	biff.AssertEqual(month.Folder, "INBOX")
	biff.AssertEqual(len(month.Messages), 2)
	biff.AssertEqual(month.Messages[0].From, "Ana Pérez <ana@example.org>")
	biff.AssertEqual(month.Messages[0].Subject, "Hola")
	biff.AssertEqual(month.Messages[0].MessageID, "<m1@example.org>")
	biff.AssertEqual(month.Messages[1].Subject, "Factura de marzo – ñandú")
	biff.AssertEqual(month.Messages[1].Attachments, []string{"informe.pdf", "año.txt"})
	md := string(h.File("INBOX/2024/2024-03/indice.md"))
	biff.AssertTrue(strings.Contains(md, "# INBOX: marzo de 2024"))
	biff.AssertTrue(strings.Contains(md, "[Factura de marzo – ñandú]("+strings.TrimPrefix(invoice, "INBOX/2024/2024-03/")+") (2 adjuntos)"))
	biff.AssertTrue(strings.Contains(string(h.File("INBOX/2024/2024-04/indice.md")), "Café con leña"))
	biff.AssertTrue(strings.Contains(string(h.File("Cafe/2024/2024-05/indice.md")), "# Café: mayo de 2024"))
	biff.AssertTrue(strings.Contains(string(h.File("Cafe/2024/2024-05/indice.md")), "Precio €"))
	assertReadOnly(t, f.log())

	// Again, with nothing new: nothing is downloaded or written.
	puts := h.NAS.Puts
	before := len(bodyFetches(f.log()))
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)
	biff.AssertEqual(len(bodyFetches(f.log())), before)
}

func TestIMAPNewMessages(t *testing.T) {
	f, h := setup(t, nil)
	biff.AssertNil(h.Run().Err)

	newer := crlf("From: ana@example.org\nSubject: Otra cosa\nDate: Sat, 16 Mar 2024 10:00:00 +0000\nMessage-ID: <m6@example.org>\n\nMás.\n")
	f.mutex.Lock()
	inbox := f.box("INBOX")
	inbox.msgs = append(inbox.msgs, fakeMessage{uid: 9, raw: newer})
	f.mutex.Unlock()
	before := len(f.log())

	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	fetches := bodyFetches(f.log()[before:])
	biff.AssertEqual(len(fetches), 1)
	biff.AssertTrue(strings.HasPrefix(fetches[0], "UID FETCH 9 "))
	var month imap.Month
	biff.AssertNil(json.Unmarshal(h.File("INBOX/2024/2024-03/indice.json"), &month))
	biff.AssertEqual(len(month.Messages), 3)
	biff.AssertEqual(month.Messages[2].Subject, "Otra cosa")
	biff.AssertTrue(h.Has(eml("INBOX/2024/2024-03", "2024-03-16", "otra-cosa", newer)))
}

func TestIMAPUIDValidityChange(t *testing.T) {
	f, h := setup(t, nil)
	biff.AssertNil(h.Run().Err)
	files := len(h.NAS.Paths("principal"))

	// The server renumbers INBOX (a migration, a rebuilt index) and a new
	// message arrives.
	newer := crlf("From: ana@example.org\nSubject: Tras el cambio\nDate: Sat, 16 Mar 2024 10:00:00 +0000\nMessage-ID: <m7@example.org>\n\nHola.\n")
	f.mutex.Lock()
	inbox := f.box("INBOX")
	inbox.validity = 2
	for i := range inbox.msgs {
		inbox.msgs[i].uid += 100
	}
	inbox.msgs = append(inbox.msgs, fakeMessage{uid: 200, raw: newer})
	f.mutex.Unlock()
	before := len(f.log())

	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertEqual(len(h.NAS.Paths("principal")), files+1) // the new .eml; the index is replaced
	// Only the new one was downloaded: the others were recognized by their
	// Message-ID. The one without Message-ID is downloaded and found written.
	fetches := bodyFetches(f.log()[before:])
	biff.AssertEqual(len(fetches), 1)
	biff.AssertEqual(strings.Fields(fetches[0])[2], "105,200")

	// And again, without the local list of what was seen: still no duplicates.
	biff.AssertNil(os.RemoveAll(h.CacheDir))
	biff.AssertNil(os.MkdirAll(h.CacheDir, 0o700))
	f.mutex.Lock()
	inbox.validity = 3
	f.mutex.Unlock()
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(len(h.NAS.Paths("principal")), files+1)
	var month imap.Month
	biff.AssertNil(json.Unmarshal(h.File("INBOX/2024/2024-03/indice.json"), &month))
	biff.AssertEqual(len(month.Messages), 3)
}

func TestIMAPInterruptedRunResumes(t *testing.T) {
	defer imap.SetBatch(1, 1)()
	f, h := setup(t, plugin.Config{"folders": "INBOX"})
	f.dropAfter = 2

	r := h.Run()
	biff.AssertNotNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	// What was copied is already in the index.
	var month imap.Month
	biff.AssertNil(json.Unmarshal(h.File("INBOX/2024/2024-03/indice.json"), &month))
	biff.AssertEqual(len(month.Messages), 2)

	f.mutex.Lock()
	f.dropAfter = 0
	f.mutex.Unlock()
	before := len(f.log())
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	// It went on from where it was: only the third message was asked.
	fetches := bodyFetches(f.log()[before:])
	biff.AssertEqual(len(fetches), 1)
	biff.AssertTrue(strings.HasPrefix(fetches[0], "UID FETCH 5 "))
	biff.AssertTrue(h.Has(eml("INBOX/2024/2024-04", "2024-04-02", "cafe-con-lena", latinMessage)))
}

func TestIMAPLimitPerRun(t *testing.T) {
	defer imap.SetBatch(50, 500)()
	_, h := setup(t, plugin.Config{"limit": "2"})
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	biff.AssertTrue(strings.Contains(strings.Join(h.Log, "\n"), "máximo"))
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
}

func TestIMAPWrongPassword(t *testing.T) {
	_, h := setup(t, plugin.Config{"password": "la de siempre"})
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "contraseña de aplicación"))
	biff.AssertTrue(strings.Contains(err.Error(), "Invalid credentials"))
	biff.AssertNotNil(h.Run().Err)
}

func TestIMAPUnknownFolder(t *testing.T) {
	_, h := setup(t, plugin.Config{"folders": "INBOX\nNo existe"})
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "«No existe»"))
	biff.AssertTrue(strings.Contains(err.Error(), "Café"))
	// Folders by the name people see.
	h.Config["folders"] = "café"
	biff.AssertNil(h.Validate())
}

func TestIMAPGmailCopiesAllMailOnce(t *testing.T) {
	f, h := setup(t, nil)
	f.gmail = true
	f.boxes = []*fakeBox{
		{name: "INBOX", attrs: `\HasNoChildren`, validity: 1, msgs: []fakeMessage{{uid: 1, raw: plainMessage}}},
		{name: "Trabajo", attrs: `\HasNoChildren`, validity: 1, msgs: []fakeMessage{{uid: 1, raw: plainMessage}}},
		{name: "[Gmail]", attrs: `\HasChildren \Noselect`, validity: 1},
		{name: "[Gmail]/Todos", attrs: `\All \HasNoChildren`, validity: 1, msgs: []fakeMessage{
			{uid: 1, raw: plainMessage, labels: []string{`"\\Inbox"`, "Trabajo", `"Caf&AOk-"`}},
			{uid: 2, raw: attachmentMessage},
		}},
		{name: "[Gmail]/Papelera", attrs: `\HasNoChildren \Trash`, validity: 1, msgs: []fakeMessage{{uid: 1, raw: trashMessage}}},
	}
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	for _, p := range h.NAS.Paths("principal") {
		biff.AssertTrue(strings.HasPrefix(p, "/Archivo/Correo/Gmail/Todos/"))
	}
	var month imap.Month
	biff.AssertNil(json.Unmarshal(h.File("Gmail/Todos/2024/2024-03/indice.json"), &month))
	biff.AssertEqual(month.Messages[0].Labels, []string{`\Inbox`, "Trabajo", "Café"})
	assertReadOnly(t, f.log())
}

func TestIMAPStartTLS(t *testing.T) {
	f, client := newFake(t, true)
	f.starttls = true
	f.boxes = standardBoxes()
	h := plugintest.New(t, imap.Plugin{}, plugin.Config{"server": "127.0.0.1:" + f.port(), "starttls": "true", "user": f.user, "password": f.pass})
	h.HTTP = client
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 4)
}

// A server without STARTTLS never gets the password.
func TestIMAPStartTLSRequired(t *testing.T) {
	f, client := newFake(t, true)
	f.boxes = standardBoxes()
	h := plugintest.New(t, imap.Plugin{}, plugin.Config{"server": "127.0.0.1", "port": f.port(), "starttls": "true", "user": f.user, "password": f.pass})
	h.HTTP = client
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "STARTTLS"))
	for _, c := range f.log() {
		biff.AssertFalse(strings.HasPrefix(c, "LOGIN"))
	}
}

// A server whose certificate is not trusted is refused.
func TestIMAPUntrustedCertificate(t *testing.T) {
	f, h := setup(t, nil)
	h.HTTP = http.DefaultClient
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "conexión segura"))
	for _, c := range f.log() {
		biff.AssertFalse(strings.HasPrefix(c, "LOGIN"))
	}
}

func TestIMAPServerPresets(t *testing.T) {
	// No server and an address of an unknown provider: it must be asked.
	h := plugintest.New(t, imap.Plugin{}, plugin.Config{"user": "ana@ejemplo.org", "password": "x"})
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "servidor"))
	h.Config = plugin.Config{"user": "ana@ejemplo.org", "password": "x", "server": "127.0.0.1", "port": "99999"}
	biff.AssertNotNil(h.Validate())
}
