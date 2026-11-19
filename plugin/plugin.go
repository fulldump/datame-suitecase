// Package plugin is the contract between the engine and each integration.
//
// An integration knows one service: which fields it needs (so the web can ask
// for them), how to check them, and how to bring what is new since last time.
// It writes plain files (JSON, Markdown, HTML and the original media) through
// an Output, with names that depend only on the content (an id, a date), never
// on when it ran: running twice writes nothing new.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Field describes one setting, for the web form.
type Field struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Help        string `json:"help,omitempty"`
	Type        string `json:"type"` // text, password, url, path, textarea, checkbox
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"` // stored encrypted, never sent back to the web
	Placeholder string `json:"placeholder,omitempty"`
	Default     string `json:"default,omitempty"`
}

// Info describes an integration.
type Info struct {
	Kind              string  `json:"kind"`
	Title             string  `json:"title"`
	Description       string  `json:"description"`
	Help              string  `json:"help"` // how to get the credentials or the export, in plain words
	DefaultFolder     string  `json:"default_folder"`
	DefaultEveryHours int     `json:"default_every_hours"`
	Fields            []Field `json:"fields"`
}

// Plugin is an integration.
type Plugin interface {
	Info() Info
	// Validate checks the settings, and the credentials if it can do it cheaply.
	Validate(ctx context.Context, env Env) error
	// Sync brings what is new. It may stop half way: what it wrote stays, and
	// the cursor it saved says where to go on.
	Sync(ctx context.Context, run *Run) error
}

// Config holds the values of the fields, secrets already decrypted.
type Config map[string]string

// Get returns a value without surrounding spaces.
func (c Config) Get(name string) string { return strings.TrimSpace(c[name]) }

// Bool reads a checkbox.
func (c Config) Bool(name string) bool {
	value, _ := strconv.ParseBool(c.Get(name))
	return value
}

// Env is what Validate and Sync can use.
type Env struct {
	Config   Config
	HTTP     *http.Client
	CacheDir string // a local folder of this integration, for working copies
	Now      func() time.Time
}

// Run is one execution of Sync.
type Run struct {
	Env
	Out Output

	cursor json.RawMessage
	save   func(json.RawMessage) error
	logf   func(string, ...any)

	mutex sync.Mutex
	items int
}

// NewRun is for the engine and for tests.
func NewRun(env Env, out Output, cursor json.RawMessage, save func(json.RawMessage) error, logf func(string, ...any)) *Run {
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.HTTP == nil {
		env.HTTP = http.DefaultClient
	}
	if save == nil {
		save = func(json.RawMessage) error { return nil }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Run{Env: env, Out: out, cursor: cursor, save: save, logf: logf}
}

// Load reads the cursor saved by the last run into v. Without one, v stays as it is.
func (r *Run) Load(v any) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if len(r.cursor) == 0 {
		return nil
	}
	return json.Unmarshal(r.cursor, v)
}

// Save keeps the cursor at once, so a run cut half way goes on from there.
func (r *Run) Save(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.mutex.Lock()
	r.cursor = data
	r.mutex.Unlock()
	return r.save(data)
}

// Cursor returns the cursor as it is now.
func (r *Run) Cursor() json.RawMessage {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.cursor
}

// Logf writes a line in the log of the integration, which the web shows.
func (r *Run) Logf(format string, args ...any) { r.logf(format, args...) }

// Item counts things saved for the first time (a post, a tweet, a message).
func (r *Run) Item(n int) {
	r.mutex.Lock()
	r.items += n
	r.mutex.Unlock()
}

// Items returns how many were counted.
func (r *Run) Items() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.items
}

// File is something to write, with a path relative to the folder of the integration.
type File struct {
	Path    string
	ModTime time.Time
	Data    []byte
	// Open and Size replace Data for big files. Open may be called more than once.
	Open func() (io.ReadCloser, error)
	Size int64
	// Mutable files may change (a month that gets more tweets, a post edited):
	// when the content differs it is replaced, and the NAS keeps the old version.
	// Other files are never replaced.
	Mutable bool
}

// Result says what happened to a file.
type Result int

const (
	Unchanged Result = iota // it was there already, the same
	Created
	Updated
	Kept // it was there with other content and is not mutable: left alone
)

// ErrNotExist is returned by Output.Read for a file that is not there.
var ErrNotExist = errors.New("no existe")

// Output writes into the folder of the integration on the NAS.
type Output interface {
	Write(ctx context.Context, file File) (Result, error)
	// Read returns a file written before, for integrations that merge (for
	// example, a month of messages that grows with each export).
	Read(ctx context.Context, path string) ([]byte, error)
}

var (
	registryMutex sync.Mutex
	registry      = map[string]Plugin{}
)

// Register makes an integration available. Each one calls it in init.
func Register(p Plugin) {
	registryMutex.Lock()
	defer registryMutex.Unlock()
	registry[p.Info().Kind] = p
}

// Get returns the integration of a kind.
func Get(kind string) (Plugin, bool) {
	registryMutex.Lock()
	defer registryMutex.Unlock()
	p, ok := registry[kind]
	return p, ok
}

// All returns every integration, by title.
func All() []Plugin {
	registryMutex.Lock()
	defer registryMutex.Unlock()
	out := make([]Plugin, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info().Title < out[j].Info().Title })
	return out
}

// CheckRequired returns an error for the first required field without a value.
func CheckRequired(info Info, config Config) error {
	for _, field := range info.Fields {
		if field.Required && config.Get(field.Name) == "" {
			return errors.New("falta «" + field.Label + "»")
		}
	}
	return nil
}
