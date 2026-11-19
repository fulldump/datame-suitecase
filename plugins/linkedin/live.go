package linkedin

// Copying LinkedIn on its own, without the person downloading anything.
//
// LinkedIn has no API for a member to read their own data. Its web, though,
// reads everything from an internal API (Voyager) authenticated with the
// session cookie, li_at. This is what is used here, with the person's own
// cookie, only to read their own profile, contacts, messages and posts.
//
// That API is not documented and changes now and then, and LinkedIn's terms do
// not allow automated access. So:
//   - every part goes on its own: if one changes, it is logged and the others
//     are still copied, and the data copy (the export) remains as a complement;
//   - it goes slowly, with pauses between requests and a maximum per run, once
//     a day by default;
//   - it stops at once when LinkedIn asks for a check, limits, or the cookie
//     expired, and says what to do.
//
// Responses come in the «normalized» form: the entities are in «included», and
// fields starting with «*» point to them by their urn. Each part turns what it
// reads into rows with the same columns as the CSVs of the export, so both
// ways write the same files and merge with each other.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

const defaultBase = "https://www.linkedin.com"

// pause is the wait between two requests: a person reading, not a robot in a
// hurry. Tests make it instant.
var pause = func(ctx context.Context) {
	wait := time.Duration(1500+rand.IntN(2500)) * time.Millisecond
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// maxRequests per run: what is left goes on the next day.
var maxRequests = 400

// Problems that stop the whole run: going on would make it worse.
var (
	errExpired  = errors.New("la cookie de LinkedIn ha caducado o no es válida: copia otra vez «li_at» desde el navegador (ver la ayuda)")
	errCheck    = errors.New("LinkedIn pide comprobar que eres tú: entra en linkedin.com desde el navegador, pasa la comprobación y copia la cookie otra vez. Se ha parado para no insistir")
	errLimited  = errors.New("LinkedIn está limitando las peticiones: se ha parado y seguirá en la próxima copia")
	errTooMany  = errors.New("se ha llegado al máximo de peticiones de esta copia: seguirá en la próxima")
	errChanged  = errors.New("LinkedIn ha cambiado esta parte de su web")
	errNoCookie = errors.New("falta la cookie «li_at»")
)

// stopsAll tells the problems after which no other part should be tried.
func stopsAll(err error) bool {
	return errors.Is(err, errExpired) || errors.Is(err, errCheck) || errors.Is(err, errLimited) || errors.Is(err, errTooMany) || errors.Is(err, context.Canceled)
}

type voyager struct {
	base     string
	cookie   string // li_at
	session  string // JSESSIONID, without quotes: also the CSRF token
	http     *http.Client
	requests int
}

func newVoyager(env plugin.Env) (*voyager, error) {
	cookie := strings.Trim(env.Config.Get("li_at"), `"`)
	if cookie == "" {
		return nil, errNoCookie
	}
	base := strings.TrimRight(env.Config.Get("base"), "/")
	if base == "" {
		base = defaultBase
	}
	// Its own client: redirects are not followed, a redirect to the login or to
	// a check is how LinkedIn says the cookie does not work.
	client := &http.Client{
		Transport: env.HTTP.Transport,
		Timeout:   60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &voyager{base: base, cookie: cookie, session: strings.Trim(env.Config.Get("jsessionid"), `"`), http: client}, nil
}

// start gets the JSESSIONID when the person did not give it: LinkedIn hands
// one to whoever comes with a valid li_at.
func (v *voyager) start(ctx context.Context) error {
	if v.session != "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.base+"/uas/authenticate", nil)
	if err != nil {
		return err
	}
	v.headers(req)
	resp, err := v.http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "JSESSIONID" {
			v.session = strings.Trim(c.Value, `"`)
		}
	}
	if v.session == "" {
		return errExpired
	}
	return nil
}

func (v *voyager) headers(req *http.Request) {
	cookies := "li_at=" + v.cookie
	if v.session != "" {
		cookies += `; JSESSIONID="` + v.session + `"`
		req.Header.Set("Csrf-Token", v.session)
	}
	req.Header.Set("Cookie", cookies)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36")
	req.Header.Set("Accept", "application/vnd.linkedin.normalized+json+2.1")
	req.Header.Set("X-Restli-Protocol-Version", "2.0.0")
	req.Header.Set("X-Li-Lang", "es_ES")
}

// get reads one page of the internal API.
func (v *voyager) get(ctx context.Context, path string) (*normalized, error) {
	if v.requests >= maxRequests {
		return nil, errTooMany
	}
	if v.requests > 0 {
		pause(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v.requests++
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.base+"/voyager/api"+path, nil)
	if err != nil {
		return nil, err
	}
	v.headers(req)
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 999 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, errLimited
	case resp.StatusCode/100 == 3:
		location := resp.Header.Get("Location")
		if strings.Contains(location, "checkpoint") || strings.Contains(location, "challenge") {
			return nil, errCheck
		}
		return nil, errExpired
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errExpired
	case resp.StatusCode == http.StatusForbidden:
		// A CSRF failure is an old JSESSIONID; anything else, a part this
		// account cannot read.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.Contains(strings.ToLower(string(body)), "csrf") {
			return nil, errExpired
		}
		return nil, fmt.Errorf("%w (403)", errChanged)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w (%d)", errChanged, resp.StatusCode)
	}
	page := &normalized{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(page); err != nil {
		return nil, fmt.Errorf("%w: %v", errChanged, err)
	}
	page.index()
	return page, nil
}

// ---- The normalized form ----

type entity map[string]any

type normalized struct {
	Data     entity   `json:"data"`
	Included []entity `json:"included"`
	byURN    map[string]entity
}

func (n *normalized) index() {
	n.byURN = map[string]entity{}
	for _, e := range n.Included {
		if urn := e.str("entityUrn"); urn != "" {
			n.byURN[urn] = e
		}
	}
}

// of returns the included entities whose $type ends with suffix
// («.Position»): the package before it has changed over the years.
func (n *normalized) of(suffix string) []entity {
	var out []entity
	for _, e := range n.Included {
		if strings.HasSuffix(e.str("$type"), suffix) {
			out = append(out, e)
		}
	}
	return out
}

// ref follows a «*field» to its entity.
func (n *normalized) ref(e entity, field string) entity {
	if e == nil {
		return nil
	}
	if urn, ok := e["*"+field].(string); ok {
		return n.byURN[urn]
	}
	if inner, ok := e[field].(map[string]any); ok {
		return inner
	}
	return nil
}

func (e entity) get(path ...string) any {
	var current any = map[string]any(e)
	for _, p := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[p]
	}
	return current
}

func (e entity) str(path ...string) string {
	switch v := e.get(path...).(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func (e entity) num(path ...string) int64 {
	if v, ok := e.get(path...).(float64); ok {
		return int64(v)
	}
	return 0
}

func (e entity) list(path ...string) []any {
	v, _ := e.get(path...).([]any)
	return v
}

// text is a field that may be a string or a «TextViewModel» ({"text": …}).
func (e entity) text(path ...string) string {
	if s := e.str(path...); s != "" {
		return s
	}
	return e.str(append(path, "text")...)
}

// ---- The cursor of the live copy ----

type liveCursor struct {
	Conversations map[string]int64 `json:"conversations,omitempty"` // last activity seen, by conversation id
	ContactsFull  int64            `json:"contacts_full,omitempty"` // when the whole list was read last
}

// ---- Copying ----

// syncLive copies what it can through the internal API. Each part on its own:
// one that changed does not stop the others.
func syncLive(ctx context.Context, run *plugin.Run, cur *liveCursor, save func() error) error {
	v, err := newVoyager(run.Env)
	if err != nil {
		return err
	}
	if err := v.start(ctx); err != nil {
		return err
	}
	me, err := v.get(ctx, "/me")
	if err != nil {
		return err
	}
	mini := me.ref(me.Data, "miniProfile")
	if mini == nil {
		return fmt.Errorf("%w: no se encuentra tu perfil", errChanged)
	}
	publicID := mini.str("publicIdentifier")
	memberID := strings.TrimPrefix(mini.str("entityUrn"), "urn:li:fs_miniProfile:")
	name := strings.TrimSpace(mini.str("firstName") + " " + mini.str("lastName"))
	run.Logf("Conectado como %s", name)

	parts := []struct {
		name string
		do   func() error
	}{
		{"el perfil", func() error { return liveProfile(ctx, run, v, publicID) }},
		{"los contactos", func() error { return liveContacts(ctx, run, v, cur, save) }},
		{"los mensajes", func() error { return liveMessages(ctx, run, v, cur, save, name) }},
		{"las publicaciones", func() error { return livePosts(ctx, run, v, memberID) }},
	}
	failed := 0
	for _, part := range parts {
		err := part.do()
		if err == nil {
			continue
		}
		if stopsAll(err) {
			return err
		}
		failed++
		run.Logf("No se han podido copiar %s: %v. Lo demás sigue; la copia de tus datos (el ZIP) también los trae", part.name, err)
	}
	if failed == len(parts) {
		return fmt.Errorf("%w: no se ha podido copiar nada", errChanged)
	}
	return nil
}

func ymd(e entity) string {
	year, month := e.num("year"), e.num("month")
	switch {
	case year == 0:
		return ""
	case month == 0:
		return strconv.FormatInt(year, 10)
	}
	return time.Date(int(year), time.Month(month), 1, 0, 0, 0, 0, time.UTC).Format("Jan 2006")
}

// period reads the dates of a position or a school, in its old form
// (timePeriod.startDate) or its new one (dateRange.start).
func period(e entity) (string, string) {
	for _, names := range [][3]string{{"timePeriod", "startDate", "endDate"}, {"dateRange", "start", "end"}} {
		if p, ok := e.get(names[0]).(map[string]any); ok {
			start, _ := p[names[1]].(map[string]any)
			end, _ := p[names[2]].(map[string]any)
			return ymd(start), ymd(end)
		}
	}
	return "", ""
}

func liveProfile(ctx context.Context, run *plugin.Run, v *voyager, publicID string) error {
	page, err := v.get(ctx, "/identity/profiles/"+url.PathEscape(publicID)+"/profileView")
	if err != nil {
		if stopsAll(err) {
			return err
		}
		// The newer form, for accounts where the old one is gone.
		page, err = v.get(ctx, "/identity/dash/profiles?q=memberIdentity&memberIdentity="+url.QueryEscape(publicID)+
			"&decorationId=com.linkedin.voyager.dash.deco.identity.profile.FullProfileWithEntities-93")
		if err != nil {
			return err
		}
	}
	profiles := page.of(".Profile")
	if len(profiles) == 0 {
		return fmt.Errorf("%w: no hay perfil en la respuesta", errChanged)
	}
	p := profiles[0]
	for _, candidate := range profiles {
		if candidate.str("publicIdentifier") == publicID {
			p = candidate
		}
	}
	profile := row{
		"First Name": p.str("firstName"), "Last Name": p.str("lastName"), "Headline": p.text("headline"),
		"Summary": p.text("summary"), "Geo Location": p.str("locationName"), "Industry": p.str("industryName"),
	}
	files := map[string][]row{}
	for _, e := range page.of(".Position") {
		start, end := period(e)
		files["positions.csv"] = append(files["positions.csv"], row{
			"Title": e.str("title"), "Company Name": e.str("companyName"), "Description": e.text("description"),
			"Location": e.str("locationName"), "Started On": start, "Finished On": end,
		})
	}
	for _, e := range page.of(".Education") {
		start, end := period(e)
		degree := e.str("degreeName")
		if f := e.str("fieldOfStudy"); f != "" {
			degree = strings.TrimSpace(strings.Trim(degree+", "+f, ", "))
		}
		files["education.csv"] = append(files["education.csv"], row{
			"School Name": e.str("schoolName"), "Degree Name": degree, "Notes": e.text("description"),
			"Activities": e.str("activities"), "Start Date": start, "End Date": end,
		})
	}
	for _, e := range page.of(".Skill") {
		files["skills.csv"] = append(files["skills.csv"], row{"Name": e.str("name")})
	}
	for _, e := range page.of(".Language") {
		files["languages.csv"] = append(files["languages.csv"], row{"Name": e.str("name"), "Proficiency": strings.ReplaceAll(strings.ToLower(e.str("proficiency")), "_", " ")})
	}
	for _, e := range page.of(".Certification") {
		start, _ := period(e)
		files["certifications.csv"] = append(files["certifications.csv"], row{
			"Name": e.str("name"), "Authority": e.str("authority"), "Started On": start, "Url": e.str("url"),
		})
	}
	read := func(name string) ([]string, []row, error) { return nil, files[name], nil }
	if err := writeProfile(ctx, run, profile, read); err != nil {
		return err
	}
	// What LinkedIn answered, as it came: if something is not rendered, it is there.
	raw, _ := json.MarshalIndent(struct {
		Data     entity   `json:"data"`
		Included []entity `json:"included"`
	}{page.Data, page.Included}, "", "  ")
	_, err = run.Out.Write(ctx, plugin.File{Path: "api/perfil.json", ModTime: run.Now(), Data: raw, Mutable: true})
	return err
}

const contactsPage = 40

func liveContacts(ctx context.Context, run *plugin.Run, v *voyager, cur *liveCursor, save func() error) error {
	// The newest first. The whole list once a week; the other days, the first
	// pages are enough to catch the new ones (the NAS keeps all the others).
	full := run.Now().Unix()-cur.ContactsFull > 7*24*3600
	maxPages := 3
	if full {
		maxPages = 1000
	}
	var rows []row
	for start := 0; start/contactsPage < maxPages; start += contactsPage {
		page, err := v.get(ctx, fmt.Sprintf("/relationships/dash/connections?decorationId=com.linkedin.voyager.dash.deco.web.mynetwork.ConnectionListWithProfile-16&count=%d&q=search&sortType=RECENTLY_ADDED&start=%d", contactsPage, start))
		if err != nil {
			if len(rows) > 0 && !stopsAll(err) {
				break
			}
			if len(rows) > 0 && stopsAll(err) {
				// What was read is written before stopping.
				if werr := writeContacts(ctx, run, rows); werr != nil {
					return werr
				}
			}
			return err
		}
		connections := page.of(".Connection")
		for _, c := range connections {
			person := page.ref(c, "connectedMemberResolutionResult")
			if person == nil {
				person = page.ref(c, "connectedMember")
			}
			if person == nil {
				continue
			}
			r := row{"First Name": person.str("firstName"), "Last Name": person.str("lastName"), "Position": person.text("headline")}
			if id := person.str("publicIdentifier"); id != "" {
				r["URL"] = "https://www.linkedin.com/in/" + id
			}
			if at := c.num("createdAt"); at > 0 {
				r["Connected On"] = time.UnixMilli(at).UTC().Format("02 Jan 2006")
			}
			rows = append(rows, r)
		}
		if len(connections) < contactsPage {
			break
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if err := writeContacts(ctx, run, rows); err != nil {
		return err
	}
	if full {
		cur.ContactsFull = run.Now().Unix()
		return save()
	}
	return nil
}

// memberName is the name of a messaging participant, through its mini profile.
func memberName(page *normalized, member entity) string {
	if member == nil {
		return ""
	}
	mini := page.ref(member, "miniProfile")
	if mini == nil {
		if inner, ok := member.get("com.linkedin.voyager.messaging.MessagingMember").(map[string]any); ok {
			mini = page.ref(entity(inner), "miniProfile")
		}
	}
	if mini == nil {
		return member.str("name")
	}
	return strings.TrimSpace(mini.str("firstName") + " " + mini.str("lastName"))
}

func liveMessages(ctx context.Context, run *plugin.Run, v *voyager, cur *liveCursor, save func() error, me string) error {
	if cur.Conversations == nil {
		cur.Conversations = map[string]int64{}
	}
	// The conversations, the most recent first, until the ones that did not
	// change since the last copy.
	type conversation struct {
		id, title string
		people    []string
		last      int64
	}
	var changed []conversation
	before := int64(0)
	for pages := 0; pages < 100; pages++ {
		path := "/messaging/conversations?keyVersion=LEGACY_INBOX&count=20"
		if before > 0 {
			path += "&createdBefore=" + strconv.FormatInt(before, 10)
		}
		page, err := v.get(ctx, path)
		if err != nil {
			return err
		}
		elements := page.Data.list("*elements")
		if len(elements) == 0 {
			break
		}
		reachedKnown := false
		for _, item := range elements {
			c := page.byURN[fmt.Sprint(item)]
			if c == nil {
				continue
			}
			id := strings.TrimPrefix(c.str("entityUrn"), "urn:li:fs_conversation:")
			last := c.num("lastActivityAt")
			if last > 0 && (before == 0 || last < before) {
				before = last
			}
			if seen := cur.Conversations[id]; seen != 0 && seen >= last {
				reachedKnown = true
				continue
			}
			conv := conversation{id: id, title: c.str("name"), last: last}
			for _, p := range c.list("*participants") {
				if n := memberName(page, page.byURN[fmt.Sprint(p)]); n != "" && !strings.EqualFold(n, me) {
					conv.people = append(conv.people, n)
				}
			}
			changed = append(changed, conv)
		}
		if reachedKnown || len(elements) < 20 {
			break
		}
	}

	for _, conv := range changed {
		var rows []row
		seen := cur.Conversations[conv.id]
		before := int64(0)
		for pages := 0; pages < 200; pages++ {
			path := "/messaging/conversations/" + url.PathEscape(conv.id) + "/events"
			if before > 0 {
				path += "?createdBefore=" + strconv.FormatInt(before, 10)
			}
			page, err := v.get(ctx, path)
			if err != nil {
				if len(rows) > 0 && stopsAll(err) {
					if werr := writeMessages(ctx, run, rows, me); werr != nil {
						return werr
					}
				}
				return err
			}
			elements := page.Data.list("*elements")
			older := false
			for _, item := range elements {
				ev := page.byURN[fmt.Sprint(item)]
				if ev == nil {
					continue
				}
				at := ev.num("createdAt")
				if at > 0 && (before == 0 || at < before) {
					before = at
				}
				if at <= seen {
					older = true // already copied in an earlier run
					continue
				}
				content, _ := ev.get("eventContent", "com.linkedin.voyager.messaging.event.MessageEvent").(map[string]any)
				if content == nil {
					continue // someone joined, a call…: not a message
				}
				msg := entity(content)
				attachments := []string{}
				for _, a := range msg.list("attachments") {
					if m, ok := a.(map[string]any); ok {
						attachments = append(attachments, entity(m).str("name"))
					}
				}
				from := memberName(page, page.ref(ev, "from"))
				to := []string{}
				for _, p := range conv.people {
					if p != from {
						to = append(to, p)
					}
				}
				if from != me && me != "" {
					to = append(to, me)
				}
				rows = append(rows, row{
					"CONVERSATION ID": conv.id, "CONVERSATION TITLE": conv.title, "FROM": from, "TO": strings.Join(to, ", "),
					"DATE": time.UnixMilli(at).UTC().Format("2006-01-02 15:04:05 MST"), "SUBJECT": msg.str("subject"),
					"CONTENT": msg.text("attributedBody"), "ATTACHMENTS": strings.Join(attachments, ", "),
				})
			}
			if older || len(elements) < 20 {
				break
			}
		}
		if len(rows) > 0 {
			if err := writeMessages(ctx, run, rows, me); err != nil {
				return err
			}
		}
		cur.Conversations[conv.id] = conv.last
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

// activityTime reads the time hidden in an activity id: its first 41 bits are
// the milliseconds since 1970, as in other ids of LinkedIn.
func activityTime(urn string) time.Time {
	id, err := strconv.ParseUint(urn[strings.LastIndex(urn, ":")+1:], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(id >> 22)).UTC()
}

func livePosts(ctx context.Context, run *plugin.Run, v *voyager, memberID string) error {
	var rows []row
	token := ""
	for start := 0; start < 2000; start += 20 {
		path := "/identity/profileUpdatesV2?count=20&includeLongTermHistory=true&moduleKey=member-shares%3Aphone&q=memberShareFeed" +
			"&profileUrn=" + url.QueryEscape("urn:li:fsd_profile:"+memberID) + "&start=" + strconv.Itoa(start)
		if token != "" {
			path += "&paginationToken=" + url.QueryEscape(token)
		}
		page, err := v.get(ctx, path)
		if err != nil {
			if len(rows) > 0 && !stopsAll(err) {
				break
			}
			return err
		}
		updates := page.of(".UpdateV2")
		for _, u := range updates {
			urn := u.str("updateMetadata", "urn")
			if urn == "" {
				continue
			}
			// Only what the person wrote: a reshare without words, or someone
			// else's post they reacted to, is not theirs.
			actor := u.str("actor", "urn")
			if actor != "" && !strings.HasSuffix(actor, memberID) {
				continue
			}
			when := activityTime(urn)
			if when.IsZero() {
				continue
			}
			r := row{
				"Date": when.Format("2006-01-02 15:04:05"), "ShareLink": "https://www.linkedin.com/feed/update/" + urn + "/",
				"ShareCommentary": u.text("commentary", "text"),
			}
			if article, ok := u.get("content", "com.linkedin.voyager.feed.render.ArticleComponent").(map[string]any); ok {
				r["SharedUrl"] = entity(article).str("navigationContext", "actionTarget")
			}
			if r["ShareCommentary"] == "" && r["SharedUrl"] == "" {
				continue
			}
			rows = append(rows, r)
		}
		token = page.Data.str("metadata", "paginationToken")
		if len(updates) < 20 {
			break
		}
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["Date"] < rows[j]["Date"] })
	return writePosts(ctx, run, rows)
}
