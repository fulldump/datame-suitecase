package dav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// maxResponse caps what is read of one answer. Address books with photos can
// be big, but a server answering without end must not fill the memory.
const maxResponse = 512 << 20

// kind tells CalDAV and CardDAV apart: they are the same protocol with a few
// different names, so everything else is shared.
type kind struct {
	name     string // caldav or carddav, also the name of the well-known URL
	prefix   string // the XML prefix used in the requests below
	homeSet  string // calendar-home-set or addressbook-home-set
	multiget string // calendar-multiget or addressbook-multiget
	data     string // calendar-data or address-data
	ext      string // the extension of each object
	folder   string // where its collections go in the NAS
	sub      string // the folder of the single objects inside a collection
	what     string // for messages: «los calendarios»
}

var (
	calendars = kind{name: "caldav", prefix: "C", homeSet: "calendar-home-set", multiget: "calendar-multiget", data: "calendar-data",
		ext: ".ics", folder: "Calendarios", sub: "eventos", what: "los calendarios"}
	contacts = kind{name: "carddav", prefix: "CR", homeSet: "addressbook-home-set", multiget: "addressbook-multiget", data: "address-data",
		ext: ".vcf", folder: "Contactos", sub: "contactos", what: "las libretas de contactos"}
)

// ---- XML ----

type multistatus struct {
	Responses []response `xml:"DAV: response"`
	SyncToken string     `xml:"DAV: sync-token"`
}

type response struct {
	Href      string     `xml:"DAV: href"`
	Status    string     `xml:"DAV: status"`
	Propstats []propstat `xml:"DAV: propstat"`
}

type propstat struct {
	Status string `xml:"DAV: status"`
	Prop   props  `xml:"DAV: prop"`
}

type hrefs struct {
	Hrefs []string `xml:"DAV: href"`
}

func (h hrefs) first() string {
	for _, href := range h.Hrefs {
		if strings.TrimSpace(href) != "" {
			return strings.TrimSpace(href)
		}
	}
	return ""
}

// props holds every property this package asks for, of both protocols:
// encoding/xml matches them by namespace, so one struct serves all answers.
type props struct {
	CurrentUserPrincipal hrefs  `xml:"DAV: current-user-principal"`
	CalendarHomeSet      hrefs  `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	AddressbookHomeSet   hrefs  `xml:"urn:ietf:params:xml:ns:carddav addressbook-home-set"`
	DisplayName          string `xml:"DAV: displayname"`
	ResourceType         struct {
		Collection  *struct{} `xml:"DAV: collection"`
		Calendar    *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
		Addressbook *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook"`
	} `xml:"DAV: resourcetype"`
	SupportedReports struct {
		Reports []struct {
			Report struct {
				SyncCollection *struct{} `xml:"DAV: sync-collection"`
			} `xml:"DAV: report"`
		} `xml:"DAV: supported-report"`
	} `xml:"DAV: supported-report-set"`
	GetETag      string `xml:"DAV: getetag"`
	CTag         string `xml:"http://calendarserver.org/ns/ getctag"`
	SyncToken    string `xml:"DAV: sync-token"`
	CalendarData string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	AddressData  string `xml:"urn:ietf:params:xml:ns:carddav address-data"`
}

func (p props) homeSet(k kind) string {
	if k.name == calendars.name {
		return p.CalendarHomeSet.first()
	}
	return p.AddressbookHomeSet.first()
}

func (p props) isCollectionOf(k kind) bool {
	if k.name == calendars.name {
		return p.ResourceType.Calendar != nil
	}
	return p.ResourceType.Addressbook != nil
}

func (p props) supportsSync() bool {
	for _, r := range p.SupportedReports.Reports {
		if r.Report.SyncCollection != nil {
			return true
		}
	}
	return false
}

func (p props) objectData(k kind) string {
	if k.name == calendars.name {
		return p.CalendarData
	}
	return p.AddressData
}

// statusCode reads «HTTP/1.1 404 Not Found».
func statusCode(status string) int {
	fields := strings.Fields(status)
	if len(fields) < 2 {
		return 0
	}
	code, _ := strconv.Atoi(fields[1])
	return code
}

// found returns the properties the server has. Those it does not have come
// in another propstat with 404, and are left out.
func (r response) found() (props, bool) {
	for _, ps := range r.Propstats {
		if code := statusCode(ps.Status); code >= 200 && code < 300 {
			return ps.Prop, true
		}
	}
	return props{}, false
}

func propfind(inner string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CR="urn:ietf:params:xml:ns:carddav" xmlns:CS="http://calendarserver.org/ns/">` +
		`<D:prop>` + inner + `</D:prop></D:propfind>`
}

func escapeXML(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// canonical is the path of a URL with one encoding, so «/a%40b/» and «/a@b/»,
// which different answers of the same server may mix, are the same key.
func canonical(u *url.URL) string {
	v := *u
	v.RawPath = ""
	return v.EscapedPath()
}

// crlf gives the line breaks iCalendar and vCard require. An XML parser turns
// them into «\n» (XML says so), so the objects that come inside a multiget
// answer are put back as the server keeps them.
func crlf(s string) []byte {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimRight(s, "\n") + "\n"
	return []byte(strings.ReplaceAll(s, "\n", "\r\n"))
}

// ---- HTTP ----

type client struct {
	http     *http.Client
	user     string
	password string
	base     *url.URL // the address the person gave: decides where the password may go
	google   bool
}

func newClient(httpClient *http.Client, s settings, base *url.URL) *client {
	// A copy that does not follow redirects by itself: Go would turn a
	// PROPFIND into a GET on a 301, and the well-known URLs of RFC 6764 are
	// exactly a redirect.
	h := *httpClient
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{http: &h, user: s.user, password: s.password, base: base, google: s.google}
}

// trusted tells whether the password may be sent to u: the same host, or one
// of the same domain (iCloud sends each account to its own pNN-caldav.icloud.com),
// and never over plain HTTP when the person gave HTTPS.
func (c *client) trusted(u *url.URL) bool {
	if c.base.Scheme == "https" && u.Scheme != "https" {
		return false
	}
	if strings.EqualFold(u.Host, c.base.Host) {
		return true
	}
	host := strings.ToLower(c.base.Hostname())
	if i := strings.IndexByte(host, '.'); i > 0 && strings.Contains(host[i+1:], ".") {
		return strings.HasSuffix(strings.ToLower(u.Hostname()), host[i:])
	}
	return false
}

type authError struct{ message string }

func (e *authError) Error() string { return e.message }

func (c *client) authError() error {
	if c.google {
		return &authError{"Google no acepta contraseñas en CalDAV ni CardDAV: solo deja entrar con OAuth, que este programa aún no sabe hacer. " +
			"Para Google Calendar usa su «dirección secreta en formato iCal» con el servicio «Fichero de una dirección web», y para los contactos, Google Takeout"}
	}
	return &authError{"el servidor no acepta el usuario o la contraseña. Comprueba el usuario (suele ser el correo completo) " +
		"y usa una contraseña de aplicación, no la normal de tu cuenta"}
}

func isAuth(err error) bool {
	var a *authError
	return errors.As(err, &a)
}

type statusError struct {
	code    int
	message string
}

func (e *statusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("el servidor respondió %d", e.code)
	}
	return fmt.Sprintf("el servidor respondió %d: %s", e.code, e.message)
}

func hasStatus(err error, codes ...int) bool {
	var s *statusError
	if !errors.As(err, &s) {
		return false
	}
	for _, code := range codes {
		if s.code == code {
			return true
		}
	}
	return false
}

// do sends one request, following redirects with the same method and body.
// It returns the URL that answered, against which the hrefs are resolved.
func (c *client) do(ctx context.Context, method string, u *url.URL, depth, body string) (*http.Response, *url.URL, error) {
	for hop := 0; hop < 8; hop++ {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
		if err != nil {
			return nil, nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
		}
		if depth != "" {
			req.Header.Set("Depth", depth)
		}
		req.Header.Set("User-Agent", "datame-suitcase")
		if c.trusted(u) {
			req.SetBasicAuth(c.user, c.password)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("no se pudo hablar con %s: %w", u.Host, err)
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location := resp.Header.Get("Location")
			resp.Body.Close()
			next, err := u.Parse(location)
			if location == "" || err != nil {
				return nil, nil, fmt.Errorf("%s redirige a una dirección no válida", u.Host)
			}
			u = next
			continue
		}
		return resp, u, nil
	}
	return nil, nil, fmt.Errorf("%s redirige demasiadas veces", u.Host)
}

func (c *client) check(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return c.authError()
	case resp.StatusCode == http.StatusNotFound:
		return &statusError{code: resp.StatusCode, message: "no existe"}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &statusError{code: resp.StatusCode, message: strings.TrimSpace(plainText(string(message)))}
	}
	return nil
}

// plainText keeps an error page short and readable in the log.
func plainText(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// query sends a PROPFIND or REPORT and reads the multistatus answer.
func (c *client) query(ctx context.Context, method string, u *url.URL, depth, body string) (*multistatus, *url.URL, error) {
	resp, final, err := c.do(ctx, method, u, depth, body)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if err := c.check(resp); err != nil {
		return nil, nil, err
	}
	var ms multistatus
	decoder := xml.NewDecoder(io.LimitReader(resp.Body, maxResponse))
	// Servers say utf-8; a few old ones say something else for the same bytes.
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) { return input, nil }
	if err := decoder.Decode(&ms); err != nil {
		return nil, nil, fmt.Errorf("%s no responde como un servidor CalDAV o CardDAV (%v)", final.Host, err)
	}
	return &ms, final, nil
}

func resolve(base *url.URL, href string) (*url.URL, error) {
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return nil, err
	}
	u := base.ResolveReference(ref)
	u.RawPath, u.Fragment, u.RawQuery = "", "", ""
	return u, nil
}

// ---- Discovery ----

// home finds where the collections of a person are: the well-known URL leads
// to the principal (who the person is for the server), and the principal says
// where its calendars or address books live.
func (c *client) home(ctx context.Context, k kind, start *url.URL) (*url.URL, error) {
	ask := propfind(`<D:current-user-principal/><` + k.prefix + `:` + k.homeSet + `/>`)
	wellKnown := start.ResolveReference(&url.URL{Path: "/.well-known/" + k.name})
	candidates := []*url.URL{start, wellKnown}
	if start.Path == "" || start.Path == "/" {
		candidates = []*url.URL{wellKnown, start}
	}
	var last error
	for _, candidate := range candidates {
		ms, final, err := c.query(ctx, "PROPFIND", candidate, "0", ask)
		if err != nil {
			if isAuth(err) || ctx.Err() != nil {
				return nil, err
			}
			last = err
			continue
		}
		var found props
		for _, r := range ms.Responses {
			if p, ok := r.found(); ok {
				found = p
				break
			}
		}
		// The address may be the principal itself.
		if home := found.homeSet(k); home != "" {
			return resolve(final, home)
		}
		principalHref := found.CurrentUserPrincipal.first()
		if principalHref == "" {
			continue
		}
		principal, err := resolve(final, principalHref)
		if err != nil {
			continue
		}
		ms, final, err = c.query(ctx, "PROPFIND", principal, "0", propfind(`<`+k.prefix+`:`+k.homeSet+`/>`))
		if err != nil {
			if isAuth(err) || ctx.Err() != nil {
				return nil, err
			}
			last = err
			continue
		}
		for _, r := range ms.Responses {
			if p, ok := r.found(); ok && p.homeSet(k) != "" {
				return resolve(final, p.homeSet(k))
			}
		}
	}
	if last != nil {
		return nil, fmt.Errorf("no encuentro %s en %s: comprueba la dirección del servidor (%v)", k.what, start.Host, last)
	}
	return nil, fmt.Errorf("no encuentro %s en %s: comprueba la dirección del servidor", k.what, start.Host)
}

// remote is a calendar or an address book as the server lists it.
type remote struct {
	url          *url.URL
	path         string
	name         string
	syncToken    string
	ctag         string
	supportsSync bool
}

func (c *client) collections(ctx context.Context, k kind, home *url.URL) ([]remote, error) {
	ms, final, err := c.query(ctx, "PROPFIND", home, "1",
		propfind(`<D:resourcetype/><D:displayname/><D:sync-token/><D:supported-report-set/><CS:getctag/>`))
	if err != nil {
		return nil, err
	}
	var out []remote
	for _, r := range ms.Responses {
		p, ok := r.found()
		if !ok || !p.isCollectionOf(k) {
			continue
		}
		u, err := resolve(final, r.Href)
		if err != nil {
			continue
		}
		path := canonical(u)
		name := strings.TrimSpace(p.DisplayName)
		if name == "" {
			name = lastSegment(path)
		}
		out = append(out, remote{url: u, path: path, name: name, syncToken: strings.TrimSpace(p.SyncToken), ctag: strings.TrimSpace(p.CTag), supportsSync: p.supportsSync()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func lastSegment(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if unescaped, err := url.PathUnescape(p); err == nil {
		return unescaped
	}
	return p
}

// ---- Changes ----

// changes is what moved in a collection since the last time.
type changes struct {
	changed map[string]string // path → etag ("" when the server does not say)
	gone    []string
	token   string
	full    bool // every object was listed: those not there are gone
}

// syncCollection asks only for the changes since a token (RFC 6578). With an
// empty token the server lists everything and gives the first token.
func (c *client) syncCollection(ctx context.Context, col remote, token string) (*changes, error) {
	ch := &changes{changed: map[string]string{}, full: token == ""}
	// A server may cut a long answer (507 on the collection itself) and expects
	// the same report again with the token it gave.
	for round := 0; round < 100; round++ {
		body := `<?xml version="1.0" encoding="utf-8"?><D:sync-collection xmlns:D="DAV:">` +
			`<D:sync-token>` + escapeXML(token) + `</D:sync-token><D:sync-level>1</D:sync-level>` +
			`<D:prop><D:getetag/><D:resourcetype/></D:prop></D:sync-collection>`
		ms, final, err := c.query(ctx, "REPORT", col.url, "0", body)
		if err != nil {
			return nil, err
		}
		truncated := false
		for _, r := range ms.Responses {
			u, err := resolve(final, r.Href)
			if err != nil {
				continue
			}
			p := canonical(u)
			if p == col.path {
				truncated = truncated || statusCode(r.Status) == http.StatusInsufficientStorage
				continue
			}
			if statusCode(r.Status) == http.StatusNotFound {
				delete(ch.changed, p)
				ch.gone = append(ch.gone, p)
				continue
			}
			found, _ := r.found()
			if found.ResourceType.Collection != nil {
				continue
			}
			ch.changed[p] = strings.TrimSpace(found.GetETag)
		}
		if ms.SyncToken != "" {
			token = ms.SyncToken
		}
		if !truncated {
			break
		}
	}
	ch.token = token
	return ch, nil
}

// etags lists every object with its ETag, for servers without sync-collection.
func (c *client) etags(ctx context.Context, col remote) (*changes, error) {
	ms, final, err := c.query(ctx, "PROPFIND", col.url, "1", propfind(`<D:getetag/><D:resourcetype/>`))
	if err != nil {
		return nil, err
	}
	ch := &changes{changed: map[string]string{}, full: true}
	for _, r := range ms.Responses {
		u, err := resolve(final, r.Href)
		if err != nil {
			continue
		}
		p := canonical(u)
		found, _ := r.found()
		if p == col.path || found.ResourceType.Collection != nil {
			continue
		}
		ch.changed[p] = strings.TrimSpace(found.GetETag)
	}
	return ch, nil
}

// ---- Objects ----

type fetched struct {
	data []byte
	etag string
}

// multiget brings many objects in one request. Objects it does not bring are
// asked one by one, and a server without multiget is asked one by one for all.
func (c *client) multiget(ctx context.Context, k kind, col remote, paths []string) (map[string]fetched, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?><` + k.prefix + `:` + k.multiget +
		` xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CR="urn:ietf:params:xml:ns:carddav">` +
		`<D:prop><D:getetag/><` + k.prefix + `:` + k.data + `/></D:prop>`)
	for _, p := range paths {
		body.WriteString(`<D:href>` + escapeXML(p) + `</D:href>`)
	}
	body.WriteString(`</` + k.prefix + `:` + k.multiget + `>`)

	out := map[string]fetched{}
	ms, final, err := c.query(ctx, "REPORT", col.url, "1", body.String())
	switch {
	case err == nil:
		for _, r := range ms.Responses {
			found, ok := r.found()
			data := found.objectData(k)
			if !ok || strings.TrimSpace(data) == "" {
				continue
			}
			u, err := resolve(final, r.Href)
			if err != nil {
				continue
			}
			out[canonical(u)] = fetched{data: crlf(data), etag: strings.TrimSpace(found.GetETag)}
		}
	case isAuth(err) || ctx.Err() != nil:
		return nil, err
	}
	for _, p := range paths {
		if _, ok := out[p]; ok {
			continue
		}
		f, err := c.get(ctx, col.url.ResolveReference(&url.URL{Path: p}))
		if hasStatus(err, http.StatusNotFound, http.StatusGone) {
			continue // deleted meanwhile: the next listing will say so
		}
		if err != nil {
			return nil, err
		}
		out[p] = f
	}
	return out, nil
}

func (c *client) get(ctx context.Context, u *url.URL) (fetched, error) {
	resp, _, err := c.do(ctx, "GET", u, "", "")
	if err != nil {
		return fetched{}, err
	}
	defer resp.Body.Close()
	if err := c.check(resp); err != nil {
		return fetched{}, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fetched{}, err
	}
	return fetched{data: data, etag: resp.Header.Get("ETag")}, nil
}
