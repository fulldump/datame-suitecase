// Package imap keeps a copy of an email account through IMAP, with an app
// password: Gmail, Outlook, iCloud, Fastmail, Yahoo or any other provider.
//
// Each message is saved as a .eml file with its bytes exactly as the server
// keeps them (RFC 822, attachments inside): the format every mail program has
// read for forty years, and plain text that can be read without any. Beside
// them, each month has an index (indice.json and indice.md) to browse it.
//
// It never changes the mailbox: folders are opened with EXAMINE (read only)
// and messages read with BODY.PEEK, so nothing is marked as read.
//
// It only brings what is new: for each folder it keeps UIDVALIDITY and the
// last UID copied. Messages are named by their content, so when the server
// renumbers a folder (UIDVALIDITY changes) it is gone through again without
// duplicating anything; a local list of the Message-IDs already copied saves
// even downloading them again.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/fulldump/datame-suitcase/plugin"
)

const defaultLimit = 5000

// batchSize is how many messages are asked in each FETCH, and
// checkpointEvery how often the indexes and the cursor are saved. Variables,
// so tests can make an interruption land between checkpoints.
var (
	batchSize       = 50
	checkpointEvery = 500
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the IMAP integration.
type Plugin struct{}

// presets are the servers of the big providers, by a name people know.
var presets = map[string]string{
	"gmail":    "imap.gmail.com",
	"google":   "imap.gmail.com",
	"outlook":  "outlook.office365.com",
	"hotmail":  "outlook.office365.com",
	"live":     "outlook.office365.com",
	"office":   "outlook.office365.com",
	"icloud":   "imap.mail.me.com",
	"apple":    "imap.mail.me.com",
	"fastmail": "imap.fastmail.com",
	"yahoo":    "imap.mail.yahoo.com",
}

// domains guess the server from the address when the field is left empty.
var domains = map[string]string{
	"gmail.com": "gmail", "googlemail.com": "gmail",
	"outlook.com": "outlook", "outlook.es": "outlook", "hotmail.com": "outlook", "hotmail.es": "outlook",
	"live.com": "outlook", "live.es": "outlook", "msn.com": "outlook",
	"icloud.com": "icloud", "me.com": "icloud", "mac.com": "icloud",
	"fastmail.com": "fastmail", "fastmail.fm": "fastmail",
	"yahoo.com": "yahoo", "yahoo.es": "yahoo", "ymail.com": "yahoo",
}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:  "imap",
		Title: "Correo (IMAP)",
		Description: "Copia tu correo de Gmail, Outlook, iCloud, Fastmail, Yahoo o cualquier otro proveedor: un fichero .eml por mensaje, con sus adjuntos, " +
			"y un índice por mes. No cambia nada en tu buzón: ni siquiera marca los correos como leídos.",
		Help: "Necesitas una «contraseña de aplicación», no la normal de tu cuenta. Casi siempre hay que activar antes la verificación en dos pasos. " +
			"Gmail: entra en myaccount.google.com/apppasswords, escribe un nombre (por ejemplo «Datame») y copia las 16 letras. " +
			"Outlook y Hotmail: account.microsoft.com → Seguridad → Opciones de seguridad avanzadas → Contraseñas de aplicación. Microsoft está retirando este acceso en muchas cuentas: si no acepta la contraseña, puede que la tuya ya no lo permita. " +
			"iCloud: account.apple.com → Inicio de sesión y seguridad → Contraseñas específicas para apps; el usuario es tu dirección @icloud.com. " +
			"Fastmail: Ajustes → Privacidad y seguridad → Contraseñas de apps. Yahoo: Seguridad de la cuenta → Generar contraseña de aplicación. " +
			"En Gmail se copia solo «Todos» (All Mail), que ya tiene todos los correos una vez, y las etiquetas de cada uno se apuntan en el índice; copiar cada etiqueta como carpeta repetiría los correos.",
		DefaultFolder:     "/Archivo/Correo",
		DefaultEveryHours: 6,
		Fields: []plugin.Field{
			{Name: "user", Label: "Dirección de correo (usuario)", Type: "text", Required: true, Placeholder: "ana@gmail.com"},
			{Name: "password", Label: "Contraseña de aplicación", Type: "password", Secret: true, Required: true, Help: "La que crea tu proveedor para programas como este (mira la ayuda), no la de tu cuenta."},
			{Name: "server", Label: "Servidor", Type: "text", Placeholder: "gmail, outlook, icloud, fastmail, yahoo o imap.ejemplo.com",
				Help: "Escribe el nombre del proveedor o la dirección del servidor IMAP. Si lo dejas vacío, se deduce de tu dirección (Gmail, Outlook, Hotmail, iCloud, Fastmail, Yahoo)."},
			{Name: "port", Label: "Puerto", Type: "text", Placeholder: "993", Help: "Vacío: 993, o 143 con STARTTLS."},
			{Name: "starttls", Label: "Usar STARTTLS en vez de TLS directo", Type: "checkbox", Default: "false",
				Help: "Solo si tu proveedor lo pide (puerto 143). La conexión va siempre cifrada: si el servidor no lo permite, no se envía la contraseña."},
			{Name: "folders", Label: "Carpetas", Type: "textarea", Placeholder: "INBOX\nEnviados",
				Help: "Una por línea, con el nombre que ves en tu correo. Vacío: todas."},
			{Name: "skip_trash", Label: "Saltar la papelera y el correo no deseado", Type: "checkbox", Default: "true"},
			{Name: "attachments", Label: "Guardar también los adjuntos sueltos", Type: "checkbox", Default: "false",
				Help: "Los adjuntos ya van dentro de cada .eml; esto los deja además como ficheros normales, en una carpeta junto a cada correo. Ocupa el doble."},
			{Name: "limit", Label: "Máximo de correos por ejecución", Type: "text", Default: strconv.Itoa(defaultLimit),
				Help: "Para no cargar al servidor: un buzón grande se copia en varias veces, y cada vez sigue donde lo dejó."},
		},
	}
}

// settings are the fields, checked.
type settings struct {
	host     string
	port     int
	starttls bool
	user     string
	password string
	folders  []string
	skip     bool
	extract  bool
	limit    int
	roots    *x509.CertPool
}

func readSettings(env plugin.Env) (settings, error) {
	c := env.Config
	s := settings{user: c.Get("user"), password: c.Get("password"), starttls: c.Bool("starttls"),
		skip: c.Get("skip_trash") != "false", extract: c.Bool("attachments"), limit: defaultLimit}
	if s.user == "" {
		return s, errors.New("falta la dirección de correo")
	}
	if s.password == "" {
		return s, errors.New("falta la contraseña de aplicación")
	}
	server := strings.ToLower(c.Get("server"))
	if server == "" {
		if at := strings.LastIndexByte(s.user, '@'); at >= 0 {
			server = domains[strings.ToLower(s.user[at+1:])]
		}
		if server == "" {
			return s, errors.New("escribe el servidor IMAP de tu proveedor (por ejemplo imap.ejemplo.com)")
		}
	}
	if host, ok := presets[server]; ok {
		server = host
	}
	port := c.Get("port")
	if host, p, found := strings.Cut(server, ":"); found && !strings.Contains(p, ":") {
		server, port = host, p
	}
	s.host = strings.Trim(server, "[]")
	switch {
	case port != "":
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return s, fmt.Errorf("el puerto «%s» no es válido", port)
		}
		s.port = n
	case s.starttls:
		s.port = 143
	default:
		s.port = 993
	}
	if limit := c.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n <= 0 {
			return s, fmt.Errorf("el máximo de correos por ejecución «%s» no es un número válido", limit)
		}
		s.limit = n
	}
	for _, line := range strings.Split(c["folders"], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s.folders = append(s.folders, line)
		}
	}
	s.roots = rootsOf(env.HTTP)
	return s, nil
}

// rootsOf takes the certificate authorities of the HTTP client, when it has
// its own, so a test server or a private CA works the same as for HTTPS.
// Without them the system ones are used. Verification is never skipped.
func rootsOf(h *http.Client) *x509.CertPool {
	if h == nil {
		return nil
	}
	if t, ok := h.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
		return t.TLSClientConfig.RootCAs
	}
	return nil
}

// connect logs in, with errors in plain words.
func connect(ctx context.Context, s settings) (*client, error) {
	c, err := dial(ctx, s.host, s.port, s.starttls, s.roots)
	if err != nil {
		return nil, err
	}
	if err := c.login(ctx, s.user, s.password); err != nil {
		c.Close()
		var auth *errAuth
		if errors.As(err, &auth) {
			return nil, fmt.Errorf("%s no acepta el usuario o la contraseña. Usa una contraseña de aplicación, no la normal de tu cuenta (mira la ayuda). El servidor dice: %s", s.host, auth.text)
		}
		return nil, fmt.Errorf("no se pudo entrar en %s: %w", s.host, err)
	}
	return c, nil
}

// Validate implements plugin.Plugin: logs in and checks the folders.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	s, err := readSettings(env)
	if err != nil {
		return err
	}
	c, err := connect(ctx, s)
	if err != nil {
		return err
	}
	defer c.Close()
	boxes, err := c.list(ctx)
	if err != nil {
		return fmt.Errorf("no se pudo leer la lista de carpetas: %w", err)
	}
	_, _, err = choose(boxes, s)
	return err
}

// isTrash tells the trash and the spam folders, by the special-use flags of
// RFC 6154 or, for servers without them, by the usual names.
func isTrash(m mailbox) bool {
	if m.Attrs[`\trash`] || m.Attrs[`\junk`] {
		return true
	}
	name := strings.ToLower(m.Display)
	if m.Delimiter != "" {
		if i := strings.LastIndex(name, strings.ToLower(m.Delimiter)); i >= 0 {
			name = name[i+len(m.Delimiter):]
		}
	}
	switch name {
	case "trash", "deleted", "deleted items", "deleted messages", "papelera", "elementos eliminados",
		"junk", "junk e-mail", "junk email", "spam", "bulk mail", "correo no deseado":
		return true
	}
	return false
}

// choose decides which folders to copy, and says why some are left out.
//
// Gmail shows each label as a folder, so a message with three labels would be
// copied three times. Gmail (and any server with a \All folder) is copied
// only through that folder, which has every message once; the labels of each
// message go to the index instead. Spam and trash are not in it, and are
// added when they are not skipped.
func choose(boxes []mailbox, s settings) ([]mailbox, string, error) {
	var selectable []mailbox
	for _, m := range boxes {
		if !m.Attrs[`\noselect`] && !m.Attrs[`\nonexistent`] {
			selectable = append(selectable, m)
		}
	}
	if len(s.folders) > 0 {
		var out []mailbox
		for _, want := range s.folders {
			found := false
			for _, m := range selectable {
				if strings.EqualFold(m.Display, want) || m.Name == want {
					out = append(out, m)
					found = true
					break
				}
			}
			if !found {
				var names []string
				for _, m := range selectable {
					names = append(names, m.Display)
				}
				return nil, "", fmt.Errorf("no existe la carpeta «%s». Hay: %s", want, strings.Join(names, ", "))
			}
		}
		return out, "", nil
	}
	var all *mailbox
	for i, m := range selectable {
		if m.Attrs[`\all`] {
			all = &selectable[i]
			break
		}
	}
	var out []mailbox
	note := ""
	if all != nil {
		out = append(out, *all)
		for _, m := range selectable {
			if !s.skip && (m.Attrs[`\trash`] || m.Attrs[`\junk`]) {
				out = append(out, m)
			}
		}
		note = fmt.Sprintf("Se copia «%s», que tiene todos los correos una vez; las demás carpetas son etiquetas y se apuntan en el índice", all.Display)
	} else {
		for _, m := range selectable {
			if s.skip && isTrash(m) {
				continue
			}
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, "", errors.New("no hay ninguna carpeta que copiar")
	}
	return out, note, nil
}

// folderPath is where a folder goes: «[Gmail]/Todos» → «Gmail/Todos».
func folderPath(m mailbox) string {
	parts := []string{m.Display}
	if m.Delimiter != "" {
		parts = strings.Split(m.Display, m.Delimiter)
	}
	var out []string
	for _, p := range parts {
		safe := strings.Trim(plugin.SafeName(strings.TrimSpace(p), 60), "_")
		if safe == "" || safe == "fichero" {
			safe = "carpeta-" + plugin.ShortHash(p)[:6]
		}
		out = append(out, safe)
	}
	return strings.Join(out, "/")
}

// folderCursor is where a folder was left.
type folderCursor struct {
	UIDValidity uint32 `json:"uidvalidity"`
	LastUID     uint32 `json:"last_uid"`
	// Rescan is set while a folder renumbered by the server is gone through
	// again; messages already copied are recognized by their Message-ID.
	Rescan bool `json:"rescan,omitempty"`
}

type cursor struct {
	Folders map[string]*folderCursor `json:"folders"` // by the server's name
}

// syncer is one run of Sync.
type syncer struct {
	run     *plugin.Run
	s       settings
	c       *client
	cur     cursor
	gmail   bool
	budget  int  // messages that may still be downloaded in this run
	limited bool // the budget ran out with messages still to copy

	months  map[string]*Month  // path of indice.json → index being filled
	pending map[string][]Entry // path of indice.json → entries not yet written
	fetched int                // messages since the last checkpoint
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	s, err := readSettings(run.Env)
	if err != nil {
		return err
	}
	y := &syncer{run: run, s: s, budget: s.limit, months: map[string]*Month{}, pending: map[string][]Entry{}}
	if err := run.Load(&y.cur); err != nil {
		return err
	}
	if y.cur.Folders == nil {
		y.cur.Folders = map[string]*folderCursor{}
	}
	if y.c, err = connect(ctx, s); err != nil {
		return err
	}
	defer y.c.Close()
	y.gmail = y.c.caps["X-GM-EXT-1"]
	boxes, err := y.c.list(ctx)
	if err != nil {
		return fmt.Errorf("no se pudo leer la lista de carpetas: %w", err)
	}
	targets, note, err := choose(boxes, s)
	if err != nil {
		return err
	}
	if note != "" {
		run.Logf("%s", note)
	}
	for _, m := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := y.folder(ctx, m)
		// Whatever was copied is indexed and remembered, even when the
		// folder stopped half way.
		if cerr := y.checkpoint(ctx); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if y.limited {
			run.Logf("Se ha llegado al máximo de %d correos en esta ejecución; seguirá en la próxima", s.limit)
			return nil
		}
	}
	return nil
}

func (y *syncer) folder(ctx context.Context, m mailbox) error {
	st, err := y.c.examine(ctx, m.Name)
	if err != nil {
		return fmt.Errorf("no se pudo abrir la carpeta «%s»: %w", m.Display, err)
	}
	fc := y.cur.Folders[m.Name]
	if fc == nil {
		fc = &folderCursor{}
		y.cur.Folders[m.Name] = fc
	}
	if fc.UIDValidity != st.UIDValidity {
		if fc.UIDValidity != 0 {
			y.run.Logf("«%s» ha cambiado de numeración en el servidor: se repasa entera, sin repetir lo ya copiado", m.Display)
			fc.Rescan = true
		}
		fc.UIDValidity, fc.LastUID = st.UIDValidity, 0
	}
	if st.Exists == 0 || (!fc.Rescan && st.UIDNext != 0 && st.UIDNext <= fc.LastUID+1) {
		fc.Rescan = false
		return nil // nothing new: not even a search is needed
	}
	uids, err := y.c.uidsAfter(ctx, fc.LastUID)
	if err != nil {
		return fmt.Errorf("no se pudo buscar en «%s»: %w", m.Display, err)
	}
	if len(uids) == 0 {
		fc.Rescan = false
		return nil
	}
	y.run.Logf("«%s»: %d correos por copiar", m.Display, len(uids))

	seen, err := y.openSeen(m.Name, fc.Rescan)
	if err != nil {
		return err
	}
	defer seen.close()

	dir := folderPath(m)
	for len(uids) > 0 {
		if y.budget <= 0 {
			y.limited = true
			return nil
		}
		n := min(batchSize, len(uids), y.budget)
		whole := uids[:n]
		uids = uids[n:]
		batch := whole
		if fc.Rescan {
			if batch, err = y.unseen(ctx, whole, seen); err != nil {
				return err
			}
		}
		if len(batch) > 0 {
			if err := y.batch(ctx, m, dir, batch, seen); err != nil {
				return err
			}
		}
		// The cursor moves only past whole batches: servers may answer a
		// FETCH in any order, so a batch cut half way is asked again (and
		// its messages, named by content, are not duplicated).
		fc.LastUID = whole[n-1]
		if y.fetched >= checkpointEvery {
			if err := y.checkpoint(ctx); err != nil {
				return err
			}
		}
	}
	fc.Rescan = false
	return nil
}

// unseen drops, while a folder is gone through again, the messages already
// copied, asking the server only for their Message-ID and size.
func (y *syncer) unseen(ctx context.Context, batch []uint32, seen *seenList) ([]uint32, error) {
	skip := map[uint32]bool{}
	err := y.c.fetch(ctx, batch, "(UID RFC822.SIZE BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)])", func(f fetched) error {
		id := messageIDOf(f.Body)
		if id != "" && seen.has(seenKey(id, f.Size)) {
			skip[f.UID] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, uid := range batch {
		if !skip[uid] {
			out = append(out, uid)
		}
	}
	return out, nil
}

func (y *syncer) batch(ctx context.Context, m mailbox, dir string, batch []uint32, seen *seenList) error {
	items := "(UID INTERNALDATE RFC822.SIZE BODY.PEEK[])"
	if y.gmail {
		items = "(UID INTERNALDATE RFC822.SIZE X-GM-LABELS BODY.PEEK[])"
	}
	y.budget -= len(batch)
	return y.c.fetch(ctx, batch, items, func(f fetched) error {
		y.fetched++
		if f.TooBig {
			y.run.Logf("«%s»: un correo de %d MB es demasiado grande y no se copia (UID %d)", m.Display, f.Size>>20, f.UID)
			return nil
		}
		return y.save(ctx, m, dir, f, seen)
	})
}

func (y *syncer) save(ctx context.Context, m mailbox, dir string, f fetched, seen *seenList) error {
	msg := parse(f.Body, f.InternalDate, y.s.extract)
	month := dir + "/" + msg.monthDir()
	name := msg.fileName()
	result, err := y.run.Out.Write(ctx, plugin.File{Path: month + "/" + name, ModTime: msg.date, Data: f.Body})
	if err != nil {
		return err
	}
	if result == plugin.Created {
		y.run.Item(1)
	}
	if y.s.extract && len(msg.attachments) > 0 {
		folder := month + "/" + strings.TrimSuffix(name, ".eml")
		for i, file := range attachmentNames(msg.attachments) {
			if _, err := y.run.Out.Write(ctx, plugin.File{Path: folder + "/" + file, ModTime: msg.date, Data: msg.attachments[i].data}); err != nil {
				return err
			}
		}
	}
	var labels []string
	for _, l := range f.Labels {
		if l != "" {
			labels = append(labels, l)
		}
	}
	index := month + "/indice.json"
	y.pending[index] = append(y.pending[index], msg.entry(name, labels))
	if _, ok := y.months[index]; !ok {
		y.months[index] = &Month{Folder: m.Display, Month: msg.date.Format("2006-01")}
	}
	if msg.messageID != "" {
		seen.add(seenKey(msg.messageID, int64(len(f.Body))))
	}
	return nil
}

// checkpoint writes the indexes touched since the last one and then saves the
// cursor, in that order: a run cut before this repeats those messages, which
// are found already written, and indexes them.
func (y *syncer) checkpoint(ctx context.Context) error {
	paths := make([]string, 0, len(y.pending))
	for p := range y.pending {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		mo := y.months[p]
		var old Month
		data, err := y.run.Out.Read(ctx, p)
		switch {
		case err == nil:
			if json.Unmarshal(data, &old) == nil {
				mo.Messages = old.Messages
			}
		case !errors.Is(err, plugin.ErrNotExist):
			return err
		}
		mo.merge(y.pending[p])
		if _, err := y.run.Out.Write(ctx, plugin.File{Path: p, ModTime: y.run.Now(), Data: plugin.JSON(mo), Mutable: true}); err != nil {
			return err
		}
		md := strings.TrimSuffix(p, ".json") + ".md"
		if _, err := y.run.Out.Write(ctx, plugin.File{Path: md, ModTime: y.run.Now(), Data: mo.markdown(), Mutable: true}); err != nil {
			return err
		}
		delete(y.pending, p)
		delete(y.months, p)
	}
	y.fetched = 0
	return y.run.Save(y.cur)
}

// messageIDOf reads the Message-ID from a block of headers.
func messageIDOf(headers []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(append(append([]byte{}, headers...), "\r\n\r\n"...)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(msg.Header.Get("Message-Id"))
}

func seenKey(messageID string, size int64) string {
	return plugin.ShortHash(strings.TrimSpace(messageID) + "|" + strconv.FormatInt(size, 10))
}

// seenList is the local list of messages already copied from a folder, by
// Message-ID and size. It is only a shortcut for when a folder is renumbered:
// if it is lost, those messages are downloaded again and found already
// written, so nothing is duplicated either.
type seenList struct {
	keys map[string]bool
	file *os.File
	w    *bufio.Writer
}

func (y *syncer) openSeen(name string, load bool) (*seenList, error) {
	l := &seenList{keys: map[string]bool{}}
	if y.run.CacheDir == "" {
		return l, nil
	}
	dir := filepath.Join(y.run.CacheDir, "vistos")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file := filepath.Join(dir, plugin.ShortHash(y.s.user+"\n"+name)+".txt")
	if load {
		if data, err := os.ReadFile(file); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if line != "" {
					l.keys[line] = true
				}
			}
		}
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l.file, l.w = f, bufio.NewWriter(f)
	return l, nil
}

func (l *seenList) has(key string) bool { return l.keys[key] }

func (l *seenList) add(key string) {
	if l.keys[key] {
		return
	}
	l.keys[key] = true
	if l.w != nil {
		l.w.WriteString(key + "\n")
	}
}

func (l *seenList) close() {
	if l.file != nil {
		l.w.Flush()
		l.file.Close()
	}
}
