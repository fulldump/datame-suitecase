// Package engine keeps the pairing with the NAS and the integrations, and runs
// each integration on its schedule or when asked.
//
// Everything is kept as one JSON file per item (github.com/holacloud/store, on
// disk), in memory while running. Credentials are sealed with the secret box
// before they reach the disk.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/holacloud/store"

	"github.com/fulldump/datame-suitcase/nas"
	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/secret"
	"github.com/fulldump/datame-suitcase/writer"
)

const (
	maxLogLines   = 200
	maxConcurrent = 2
	retryAfter    = time.Hour // after a failure, sooner than the usual schedule
	pairingID     = "nas"
)

// Pairing is the NAS this program writes into.
type Pairing struct {
	*store.Id
	Addresses     []string `json:"addresses"`
	Pin           string   `json:"pin"`
	Token         string   `json:"token"` // sealed
	Name          string   `json:"name"`
	DefaultVolume string   `json:"default_volume"`
	DefaultFolder string   `json:"default_folder"`
	LastAddress   string   `json:"last_address"`
	Paired        int64    `json:"paired"`
}

// Integration is one service to keep a copy of, and where.
type Integration struct {
	*store.Id
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Volume     string            `json:"volume"`
	Folder     string            `json:"folder"`
	EveryHours int               `json:"every_hours"`
	Enabled    bool              `json:"enabled"`
	Config     map[string]string `json:"config"`  // fields that are not secret
	Secrets    string            `json:"secrets"` // sealed JSON of the secret fields
	Created    int64             `json:"created"`
}

// LogLine is a line of the log of an integration.
type LogLine struct {
	Time int64  `json:"time"`
	Text string `json:"text"`
}

// State is what an integration remembers between runs.
type State struct {
	*store.Id
	Cursor     json.RawMessage   `json:"cursor,omitempty"`
	Manifest   map[string]string `json:"manifest,omitempty"`
	LastStart  int64             `json:"last_start"`
	LastEnd    int64             `json:"last_end"`
	LastOK     int64             `json:"last_ok"`
	LastError  string            `json:"last_error"`
	LastItems  int               `json:"last_items"`
	LastFiles  writer.Stats      `json:"last_files"`
	TotalItems int               `json:"total_items"`
	TotalFiles int               `json:"total_files"`
	Log        []LogLine         `json:"log"`
}

// Engine is the core of the program.
type Engine struct {
	dir      string
	secrets  *secret.Box
	http     *http.Client
	now      func() time.Time
	pairings *store.StoreDisk[Pairing]
	configs  *store.StoreDisk[Integration]
	states   *store.StoreDisk[State]

	mutex        sync.Mutex
	pairing      *Pairing
	client       *nas.Client
	integrations map[string]*Integration
	state        map[string]*State
	running      map[string]context.CancelFunc
	slots        chan struct{}
	wake         chan struct{}
	wg           sync.WaitGroup
}

// Options for New.
type Options struct {
	Dir     string // data folder
	KeyFile string // defaults to <Dir>/clave
	HTTP    *http.Client
	Now     func() time.Time
}

// New opens the data folder, creating it the first time.
func New(options Options) (*Engine, error) {
	// Absolute from the start: plugins run tools like git in other directories,
	// where a relative path would point somewhere else.
	dir, err := filepath.Abs(options.Dir)
	if err != nil {
		return nil, err
	}
	options.Dir = dir
	if err := os.MkdirAll(options.Dir, 0o700); err != nil {
		return nil, err
	}
	keyFile := options.KeyFile
	if keyFile == "" {
		keyFile = filepath.Join(options.Dir, "clave")
	}
	if keyFile, err = filepath.Abs(keyFile); err != nil {
		return nil, err
	}
	box, err := secret.LoadOrCreate(keyFile)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		dir:          options.Dir,
		secrets:      box,
		http:         options.HTTP,
		now:          options.Now,
		integrations: map[string]*Integration{},
		state:        map[string]*State{},
		running:      map[string]context.CancelFunc{},
		slots:        make(chan struct{}, maxConcurrent),
		wake:         make(chan struct{}, 1),
	}
	if e.http == nil {
		e.http = &http.Client{Timeout: 15 * time.Minute, Transport: userAgent{http.DefaultTransport}}
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.pairings, err = store.NewStoreDisk[Pairing](filepath.Join(options.Dir, "nas")); err != nil {
		return nil, err
	}
	if e.configs, err = store.NewStoreDisk[Integration](filepath.Join(options.Dir, "integraciones")); err != nil {
		return nil, err
	}
	if e.states, err = store.NewStoreDisk[State](filepath.Join(options.Dir, "estado")); err != nil {
		return nil, err
	}
	ctx := context.Background()
	if e.pairing, err = e.pairings.Get(ctx, pairingID); err != nil {
		return nil, err
	}
	if e.pairing != nil {
		if e.client, err = e.newClient(e.pairing); err != nil {
			return nil, err
		}
	}
	list, err := e.configs.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range list {
		e.integrations[item.Id.Id] = item
		state, err := e.states.Get(ctx, item.Id.Id)
		if err != nil {
			return nil, err
		}
		if state == nil {
			state = &State{Id: store.NewId(item.Id.Id)}
		}
		e.state[item.Id.Id] = state
	}
	return e, nil
}

type userAgent struct{ next http.RoundTripper }

func (u userAgent) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("User-Agent") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("User-Agent", "datame-suitcase/0.1 (copia personal; +https://github.com/fulldump/datame-suitcase)")
	}
	return u.next.RoundTrip(r)
}

func newID() string {
	raw := make([]byte, 6)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

func (e *Engine) newClient(p *Pairing) (*nas.Client, error) {
	token, err := e.secrets.Open(p.Token)
	if err != nil {
		return nil, err
	}
	return nas.NewClient(nas.Code{Version: 1, Addresses: p.Addresses, Pin: p.Pin, Token: token}, p.LastAddress), nil
}

// ---- Pairing ----

// NASView is what the web shows about the NAS.
type NASView struct {
	Paired        bool         `json:"paired"`
	Name          string       `json:"name,omitempty"`
	Addresses     []string     `json:"addresses,omitempty"`
	LastAddress   string       `json:"last_address,omitempty"`
	DefaultVolume string       `json:"default_volume,omitempty"`
	DefaultFolder string       `json:"default_folder,omitempty"`
	User          string       `json:"user,omitempty"`
	Volumes       []nas.Volume `json:"volumes"`
	Active        bool         `json:"active"`
	Reachable     bool         `json:"reachable"`
	Error         string       `json:"error,omitempty"`
}

// Pair checks a code against its NAS and keeps it.
func (e *Engine) Pair(ctx context.Context, text string) (NASView, error) {
	code, err := nas.ParseCode(text)
	if err != nil {
		return NASView{}, err
	}
	client := nas.NewClient(code, "")
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	me, err := client.Me(ctx)
	if err != nil {
		return NASView{}, err
	}
	sealed, err := e.secrets.Seal(code.Token)
	if err != nil {
		return NASView{}, err
	}
	p := &Pairing{Id: store.NewId(pairingID), Addresses: code.Addresses, Pin: code.Pin, Token: sealed, Name: code.Name,
		DefaultVolume: code.Volume, DefaultFolder: code.Folder, LastAddress: client.LastAddress(), Paired: e.now().Unix()}
	if err := e.pairings.Put(ctx, p); err != nil {
		return NASView{}, err
	}
	e.mutex.Lock()
	e.pairing, e.client = p, client
	e.mutex.Unlock()
	return e.view(p, me, nil), nil
}

// Unpair forgets the NAS. The integrations stay, waiting for a new pairing.
func (e *Engine) Unpair(ctx context.Context) error {
	e.mutex.Lock()
	e.pairing, e.client = nil, nil
	e.mutex.Unlock()
	return e.pairings.Delete(ctx, pairingID)
}

func (e *Engine) view(p *Pairing, me nas.Me, err error) NASView {
	v := NASView{Paired: true, Name: p.Name, Addresses: p.Addresses, LastAddress: p.LastAddress, DefaultVolume: p.DefaultVolume, DefaultFolder: p.DefaultFolder, Volumes: []nas.Volume{}}
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Reachable, v.Active, v.User = true, me.Active, me.Name
	if me.Volumes != nil {
		v.Volumes = me.Volumes
	}
	return v
}

// NAS asks the NAS how it is.
func (e *Engine) NAS(ctx context.Context) NASView {
	e.mutex.Lock()
	p, client := e.pairing, e.client
	e.mutex.Unlock()
	if p == nil {
		return NASView{Volumes: []nas.Volume{}}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	me, err := client.Me(ctx)
	e.rememberAddress(client)
	return e.view(p, me, err)
}

// rememberAddress keeps the address that answered, so the next start tries it first.
func (e *Engine) rememberAddress(client *nas.Client) {
	e.mutex.Lock()
	p := e.pairing
	changed := p != nil && e.client == client && client.LastAddress() != "" && client.LastAddress() != p.LastAddress
	if changed {
		p.LastAddress = client.LastAddress()
	}
	e.mutex.Unlock()
	if changed {
		if err := e.pairings.Put(context.Background(), p); err != nil {
			log.Printf("save pairing: %s", err)
		}
	}
}

// ---- Integrations ----

// Input is what the web sends to create or change an integration.
type Input struct {
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Volume     string            `json:"volume"`
	Folder     string            `json:"folder"`
	EveryHours int               `json:"every_hours"`
	Enabled    bool              `json:"enabled"`
	Config     map[string]string `json:"config"`
}

// View is what the web shows of an integration.
type View struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Service     string            `json:"service"`
	Name        string            `json:"name"`
	Volume      string            `json:"volume"`
	Folder      string            `json:"folder"`
	EveryHours  int               `json:"every_hours"`
	Enabled     bool              `json:"enabled"`
	Config      map[string]string `json:"config"`
	SecretsSet  []string          `json:"secrets_set"`
	Running     bool              `json:"running"`
	NextRun     int64             `json:"next_run"`
	LastStart   int64             `json:"last_start"`
	LastEnd     int64             `json:"last_end"`
	LastOK      int64             `json:"last_ok"`
	LastError   string            `json:"last_error"`
	LastItems   int               `json:"last_items"`
	LastFiles   writer.Stats      `json:"last_files"`
	TotalItems  int               `json:"total_items"`
	TotalFiles  int               `json:"total_files"`
	Created     int64             `json:"created"`
	UnknownKind bool              `json:"unknown_kind,omitempty"`
}

// ErrNotFound means there is no integration with that id.
var ErrNotFound = errors.New("esa integración no existe")

// ValidationError is a problem with what the person typed.
type ValidationError struct{ Err error }

func (v ValidationError) Error() string { return v.Err.Error() }
func (v ValidationError) Unwrap() error { return v.Err }

func (e *Engine) secretsOf(item *Integration) (map[string]string, error) {
	out := map[string]string{}
	plain, err := e.secrets.Open(item.Secrets)
	if err != nil || plain == "" {
		return out, err
	}
	return out, json.Unmarshal([]byte(plain), &out)
}

func (e *Engine) config(item *Integration) (plugin.Config, error) {
	secrets, err := e.secretsOf(item)
	if err != nil {
		return nil, err
	}
	config := plugin.Config{}
	for k, v := range item.Config {
		config[k] = v
	}
	for k, v := range secrets {
		config[k] = v
	}
	return config, nil
}

func (e *Engine) cacheDir(id string) string { return filepath.Join(e.dir, "cache", id) }

// List returns every integration with its state.
func (e *Engine) List() []View {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	out := make([]View, 0, len(e.integrations))
	for id := range e.integrations {
		out = append(out, e.viewLocked(id))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns one integration.
func (e *Engine) Get(id string) (View, error) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if e.integrations[id] == nil {
		return View{}, ErrNotFound
	}
	return e.viewLocked(id), nil
}

func (e *Engine) viewLocked(id string) View {
	item, state := e.integrations[id], e.state[id]
	v := View{ID: id, Kind: item.Kind, Name: item.Name, Volume: item.Volume, Folder: item.Folder, EveryHours: item.EveryHours,
		Enabled: item.Enabled, Config: map[string]string{}, SecretsSet: []string{}, Created: item.Created,
		LastStart: state.LastStart, LastEnd: state.LastEnd, LastOK: state.LastOK, LastError: state.LastError,
		LastItems: state.LastItems, LastFiles: state.LastFiles, TotalItems: state.TotalItems, TotalFiles: state.TotalFiles}
	for k, value := range item.Config {
		v.Config[k] = value
	}
	if p, ok := plugin.Get(item.Kind); ok {
		v.Service = p.Info().Title
		// Which secrets are set, without decrypting them: the web only needs to
		// know whether to say «guardado».
		if secrets, err := e.secretsOf(item); err == nil {
			for _, field := range p.Info().Fields {
				if field.Secret && secrets[field.Name] != "" {
					v.SecretsSet = append(v.SecretsSet, field.Name)
				}
			}
		}
	} else {
		v.Service, v.UnknownKind = item.Kind, true
	}
	_, v.Running = e.running[id]
	if item.Enabled {
		v.NextRun = e.nextRunLocked(item, state).Unix()
	}
	return v
}

func (e *Engine) nextRunLocked(item *Integration, state *State) time.Time {
	if state.LastStart == 0 {
		return time.Unix(item.Created, 0)
	}
	every := time.Duration(max(item.EveryHours, 1)) * time.Hour
	if state.LastError != "" && retryAfter < every {
		every = retryAfter
	}
	return time.Unix(state.LastStart, 0).Add(every)
}

// prepare checks an input and builds the integration from it. previous is the
// integration being changed, if any: secret fields left empty keep its value.
func (e *Engine) prepare(ctx context.Context, input Input, previous *Integration) (*Integration, error) {
	p, ok := plugin.Get(input.Kind)
	if !ok {
		return nil, ValidationError{errors.New("servicio desconocido")}
	}
	info := p.Info()
	item := &Integration{Kind: input.Kind, Name: strings.TrimSpace(input.Name), Volume: input.Volume,
		Folder: "/" + strings.Trim(strings.TrimSpace(input.Folder), "/"), EveryHours: input.EveryHours, Enabled: input.Enabled, Config: map[string]string{}}
	if item.Name == "" {
		item.Name = info.Title
	}
	if len(item.Name) > 80 {
		return nil, ValidationError{errors.New("el nombre es demasiado largo")}
	}
	if item.Volume == "" {
		return nil, ValidationError{errors.New("elige el volumen del NAS donde guardar")}
	}
	if item.Folder == "/" || strings.Contains(item.Folder, "..") {
		return nil, ValidationError{errors.New("elige una carpeta dentro del volumen, por ejemplo " + info.DefaultFolder)}
	}
	if item.EveryHours <= 0 {
		item.EveryHours = max(info.DefaultEveryHours, 1)
	}
	var oldSecrets map[string]string
	if previous != nil {
		var err error
		if oldSecrets, err = e.secretsOf(previous); err != nil {
			return nil, err
		}
	}
	secrets := map[string]string{}
	config := plugin.Config{}
	for _, field := range info.Fields {
		value := strings.TrimSpace(input.Config[field.Name])
		if field.Type == "textarea" {
			value = strings.TrimRight(input.Config[field.Name], " \n")
		}
		if field.Secret {
			if value == "" {
				value = oldSecrets[field.Name]
			}
			if value != "" {
				secrets[field.Name] = value
			}
		} else if value != "" {
			item.Config[field.Name] = value
		}
		config[field.Name] = value
	}
	if err := plugin.CheckRequired(info, config); err != nil {
		return nil, ValidationError{err}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.Validate(ctx, e.env(config, "")); err != nil {
		return nil, ValidationError{err}
	}
	if len(secrets) > 0 {
		data, _ := json.Marshal(secrets)
		sealed, err := e.secrets.Seal(string(data))
		if err != nil {
			return nil, err
		}
		item.Secrets = sealed
	}
	return item, nil
}

func (e *Engine) env(config plugin.Config, cacheDir string) plugin.Env {
	return plugin.Env{Config: config, HTTP: e.http, CacheDir: cacheDir, Now: e.now}
}

// Create adds an integration.
func (e *Engine) Create(ctx context.Context, input Input) (View, error) {
	item, err := e.prepare(ctx, input, nil)
	if err != nil {
		return View{}, err
	}
	item.Id = store.NewId(newID())
	item.Created = e.now().Unix()
	state := &State{Id: store.NewId(item.Id.Id)}
	if err := e.configs.Put(ctx, item); err != nil {
		return View{}, err
	}
	if err := e.states.Put(ctx, state); err != nil {
		return View{}, err
	}
	e.mutex.Lock()
	e.integrations[item.Id.Id], e.state[item.Id.Id] = item, state
	e.mutex.Unlock()
	e.poke()
	return e.Get(item.Id.Id)
}

// Update changes an integration. Its state (cursor, counts) stays, unless the
// service changes.
func (e *Engine) Update(ctx context.Context, id string, input Input) (View, error) {
	e.mutex.Lock()
	previous := e.integrations[id]
	e.mutex.Unlock()
	if previous == nil {
		return View{}, ErrNotFound
	}
	if input.Kind == "" {
		input.Kind = previous.Kind
	}
	if input.Kind != previous.Kind {
		return View{}, ValidationError{errors.New("no se puede cambiar el servicio: crea otra integración")}
	}
	item, err := e.prepare(ctx, input, previous)
	if err != nil {
		return View{}, err
	}
	item.Id, item.Created = previous.Id, previous.Created
	if err := e.configs.Put(ctx, item); err != nil {
		return View{}, err
	}
	e.mutex.Lock()
	e.integrations[id] = item
	// Another folder means another place: what the manifest says was written
	// is not there.
	if state := e.state[id]; previous.Volume != item.Volume || previous.Folder != item.Folder {
		state.Manifest = nil
	}
	e.mutex.Unlock()
	e.saveState(id)
	e.poke()
	return e.Get(id)
}

// Delete removes an integration and its local state. What it wrote in the NAS stays.
func (e *Engine) Delete(ctx context.Context, id string) error {
	e.mutex.Lock()
	if e.integrations[id] == nil {
		e.mutex.Unlock()
		return ErrNotFound
	}
	if cancel := e.running[id]; cancel != nil {
		cancel()
	}
	delete(e.integrations, id)
	delete(e.state, id)
	e.mutex.Unlock()
	if err := e.configs.Delete(ctx, id); err != nil {
		return err
	}
	if err := e.states.Delete(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(e.cacheDir(id))
}

// Log returns the last lines of the log of an integration.
func (e *Engine) Log(id string) ([]LogLine, error) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	state := e.state[id]
	if state == nil {
		return nil, ErrNotFound
	}
	return append([]LogLine{}, state.Log...), nil
}

// ---- Running ----

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Loop runs the integrations when they are due, until ctx ends.
func (e *Engine) Loop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		e.startDue(ctx)
		select {
		case <-ctx.Done():
			e.mutex.Lock()
			for _, cancel := range e.running {
				cancel()
			}
			e.mutex.Unlock()
			e.wg.Wait()
			return
		case <-ticker.C:
		case <-e.wake:
		}
	}
}

func (e *Engine) startDue(ctx context.Context) {
	now := e.now()
	e.mutex.Lock()
	due := []string{}
	if e.client != nil {
		for id, item := range e.integrations {
			if _, running := e.running[id]; !running && item.Enabled && !now.Before(e.nextRunLocked(item, e.state[id])) {
				due = append(due, id)
			}
		}
	}
	e.mutex.Unlock()
	for _, id := range due {
		e.Start(ctx, id)
	}
}

// Start runs an integration now, in the background. It returns false if it was
// running already.
func (e *Engine) Start(ctx context.Context, id string) (bool, error) {
	e.mutex.Lock()
	if e.integrations[id] == nil {
		e.mutex.Unlock()
		return false, ErrNotFound
	}
	if _, running := e.running[id]; running {
		e.mutex.Unlock()
		return false, nil
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.running[id] = cancel
	e.wg.Add(1)
	e.mutex.Unlock()
	go func() {
		defer e.wg.Done()
		defer cancel()
		e.slots <- struct{}{}
		defer func() { <-e.slots }()
		e.runOne(ctx, id)
	}()
	return true, nil
}

// RunNow runs an integration and waits for it. For tests and the command line.
func (e *Engine) RunNow(ctx context.Context, id string) (View, error) {
	e.mutex.Lock()
	if e.integrations[id] == nil {
		e.mutex.Unlock()
		return View{}, ErrNotFound
	}
	if _, running := e.running[id]; running {
		e.mutex.Unlock()
		return View{}, errors.New("ya se está ejecutando")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.running[id] = cancel
	e.mutex.Unlock()
	e.runOne(ctx, id)
	return e.Get(id)
}

func (e *Engine) logf(id string, format string, args ...any) {
	line := LogLine{Time: e.now().Unix(), Text: fmt.Sprintf(format, args...)}
	e.mutex.Lock()
	defer e.mutex.Unlock()
	state := e.state[id]
	if state == nil {
		return
	}
	state.Log = append(state.Log, line)
	if len(state.Log) > maxLogLines {
		state.Log = append([]LogLine(nil), state.Log[len(state.Log)-maxLogLines:]...)
	}
}

func (e *Engine) saveState(id string) {
	e.mutex.Lock()
	state := e.state[id]
	var copy State
	if state != nil {
		copy = *state
		copy.Manifest = make(map[string]string, len(state.Manifest))
		for k, v := range state.Manifest {
			copy.Manifest[k] = v
		}
		copy.Log = append([]LogLine(nil), state.Log...)
	}
	e.mutex.Unlock()
	if state == nil {
		return // deleted meanwhile
	}
	if err := e.states.Put(context.Background(), &copy); err != nil {
		log.Printf("save state of %s: %s", id, err)
	}
}

// friendly turns an error into words for the web.
func friendly(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "se detuvo antes de terminar; seguirá donde lo dejó"
	case errors.Is(err, context.DeadlineExceeded):
		return "tardó demasiado; seguirá donde lo dejó"
	}
	return err.Error()
}

func (e *Engine) runOne(ctx context.Context, id string) {
	defer func() {
		e.mutex.Lock()
		delete(e.running, id)
		e.mutex.Unlock()
	}()

	e.mutex.Lock()
	item, state, client := e.integrations[id], e.state[id], e.client
	if item == nil {
		e.mutex.Unlock()
		return
	}
	state.LastStart = e.now().Unix()
	manifest := make(map[string]string, len(state.Manifest))
	for k, v := range state.Manifest {
		manifest[k] = v
	}
	cursor := state.Cursor
	e.mutex.Unlock()

	logf := func(format string, args ...any) { e.logf(id, format, args...) }
	logf("Empieza la copia")

	var stats writer.Stats
	items := 0
	err := func() error {
		p, ok := plugin.Get(item.Kind)
		if !ok {
			return fmt.Errorf("este programa no conoce el servicio «%s»", item.Kind)
		}
		if client == nil {
			return errors.New("falta emparejar con el NAS")
		}
		config, err := e.config(item)
		if err != nil {
			return err
		}
		cache := e.cacheDir(id)
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return err
		}
		w := writer.New(client, item.Volume, item.Folder, manifest, logf)
		checkpoint := func(cursor json.RawMessage) error {
			snapshot := w.Manifest()
			e.mutex.Lock()
			if s := e.state[id]; s != nil {
				if cursor != nil {
					s.Cursor = cursor
				}
				s.Manifest = snapshot
			}
			e.mutex.Unlock()
			e.saveState(id)
			return nil
		}
		w.OnProgress(func() { checkpoint(nil) })
		run := plugin.NewRun(e.env(config, cache), w, cursor, checkpoint, logf)
		err = p.Sync(ctx, run)
		e.rememberAddress(client)
		checkpoint(run.Cursor())
		stats, items = w.Stats(), run.Items()
		return err
	}()

	e.mutex.Lock()
	if state = e.state[id]; state != nil {
		state.LastEnd = e.now().Unix()
		state.LastItems, state.LastFiles = items, stats
		state.TotalItems += items
		state.TotalFiles += stats.Created + stats.Updated
		if err != nil {
			state.LastError = friendly(err)
		} else {
			state.LastError, state.LastOK = "", state.LastEnd
		}
	}
	e.mutex.Unlock()
	if err != nil {
		logf("Error: %s", friendly(err))
	} else {
		logf("Terminado: %d elementos nuevos, %d ficheros nuevos, %d actualizados, %d sin cambios", items, stats.Created, stats.Updated, stats.Unchanged)
	}
	e.saveState(id)
}
