// Package github keeps a copy of a GitHub account: each repository as a git
// bundle (one file that `git clone` understands), its issues and pull requests
// with their comments as JSON and Markdown, and the starred repositories.
//
// The code goes through the git command, which must be installed: a bare
// mirror is kept in the local cache and only fetches what changed; the bundle
// is made again and sent only when some branch or tag moved.
package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

const defaultAPI = "https://api.github.com"

func init() { plugin.Register(Plugin{}) }

// Plugin is the GitHub integration.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:        "github",
		Title:       "GitHub",
		Description: "Copia tus repositorios (como paquetes de git que se clonan sin GitHub), sus issues y pull requests con los comentarios, y tus estrellas.",
		Help: "Crea un token en GitHub → Settings → Developer settings → Personal access tokens → Fine-grained tokens. " +
			"Dale acceso a tus repositorios con permisos de solo lectura: Contents, Issues, Pull requests y Metadata. " +
			"Sin token solo se copian los repositorios públicos y la API permite muy pocas peticiones por hora. " +
			"Para copiar el código hace falta tener git instalado en la máquina donde corre este programa.",
		DefaultFolder:     "/Archivo/GitHub",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "user", Label: "Usuario de GitHub", Type: "text", Required: true, Placeholder: "fulldump"},
			{Name: "token", Label: "Token personal (solo lectura)", Type: "password", Secret: true, Help: "Opcional, pero necesario para los repositorios privados."},
			{Name: "code", Label: "Copiar el código de los repositorios (git)", Type: "checkbox", Default: "true"},
			{Name: "issues", Label: "Copiar issues y pull requests", Type: "checkbox", Default: "true"},
			{Name: "stars", Label: "Copiar la lista de estrellas", Type: "checkbox", Default: "true"},
			{Name: "forks", Label: "Incluir los forks", Type: "checkbox", Default: "false"},
			{Name: "api", Label: "Dirección de la API", Type: "url", Default: defaultAPI, Help: "Solo cambia para GitHub Enterprise."},
		},
	}
}

type client struct {
	http  *http.Client
	api   string
	token string
}

func newClient(env plugin.Env) *client {
	api := strings.TrimRight(env.Config.Get("api"), "/")
	if api == "" {
		api = defaultAPI
	}
	return &client{http: env.HTTP, api: api, token: env.Config.Get("token")}
}

var errGone = errors.New("gone")

// get reads one page of the API. It returns the URL of the next page, if any.
func (c *client) get(ctx context.Context, uri string, value any) (string, error) {
	if !strings.HasPrefix(uri, "http") {
		uri = c.api + uri
	}
	req, err := http.NewRequestWithContext(ctx, "GET", uri, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("no se pudo hablar con GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized:
		return "", errors.New("GitHub no acepta el token: puede haber caducado")
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && resp.Header.Get("X-RateLimit-Remaining") == "0":
		reset := ""
		if seconds, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			reset = " hasta las " + time.Unix(seconds, 0).Format("15:04")
		}
		return "", fmt.Errorf("se ha agotado el límite de peticiones de GitHub%s; seguirá en la próxima ejecución (con un token el límite es mucho mayor)", reset)
	case resp.StatusCode == http.StatusNotFound:
		return "", plugin.ErrNotExist
	case resp.StatusCode == http.StatusGone:
		return "", errGone
	default:
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("GitHub respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(resp.Body).Decode(value); err != nil {
		return "", err
	}
	return nextPage(resp.Header.Get("Link")), nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextPage(header string) string {
	if m := linkNext.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

// all reads every page of a list.
func all[T any](ctx context.Context, c *client, uri string) ([]T, error) {
	out := []T{}
	for uri != "" {
		var page []T
		next, err := c.get(ctx, uri, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		uri = next
	}
	return out, nil
}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	c := newClient(env)
	user := env.Config.Get("user")
	var profile struct {
		Login string `json:"login"`
	}
	if _, err := c.get(ctx, "/users/"+url.PathEscape(user), &profile); err != nil {
		if errors.Is(err, plugin.ErrNotExist) {
			return fmt.Errorf("no existe el usuario %s en GitHub", user)
		}
		return err
	}
	if c.token != "" {
		var me struct {
			Login string `json:"login"`
		}
		if _, err := c.get(ctx, "/user", &me); err != nil {
			return err
		}
		if !strings.EqualFold(me.Login, user) {
			return fmt.Errorf("el token es de %s, no de %s", me.Login, user)
		}
	}
	if env.Config.Get("code") != "false" {
		if _, err := exec.LookPath("git"); err != nil {
			return errors.New("para copiar el código hace falta git: instálalo (sudo apt install git) o desmarca esa opción")
		}
	}
	return nil
}

type repo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	Fork     bool   `json:"fork"`
	Private  bool   `json:"private"`
	Size     int64  `json:"size"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type cursor struct {
	Refs        map[string]string `json:"refs"`         // repo → hash of its refs when the bundle was sent
	IssuesSince map[string]string `json:"issues_since"` // repo → updated_at of the last issue seen
	Known       map[string]bool   `json:"known"`        // repos seen, to count the new ones
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	var cur cursor
	if err := run.Load(&cur); err != nil {
		return err
	}
	if cur.Refs == nil {
		cur.Refs = map[string]string{}
	}
	if cur.IssuesSince == nil {
		cur.IssuesSince = map[string]string{}
	}
	if cur.Known == nil {
		cur.Known = map[string]bool{}
	}
	c := newClient(run.Env)
	user := run.Config.Get("user")

	listURI := "/users/" + url.PathEscape(user) + "/repos?type=owner&per_page=100"
	if c.token != "" {
		listURI = "/user/repos?affiliation=owner&per_page=100" // includes the private ones
	}
	var raw []json.RawMessage
	raw, err := all[json.RawMessage](ctx, c, listURI)
	if err != nil {
		return err
	}

	forks := 0
	defer func() {
		if forks > 0 {
			run.Logf("%d forks sin copiar: actívalo en «Incluir los forks» si los quieres", forks)
		}
	}()
	run.Logf("%d repositorios", len(raw))

	var problems []string
	for _, item := range raw {
		if err := ctx.Err(); err != nil {
			return err
		}
		var r repo
		if err := json.Unmarshal(item, &r); err != nil {
			continue
		}
		if r.Fork && run.Config.Get("forks") != "true" {
			forks++
			continue
		}
		folder := "repos/" + plugin.SafeName(r.Name, 100)
		if _, err := run.Out.Write(ctx, plugin.File{Path: folder + "/repo.json", ModTime: run.Now(), Data: plugin.JSON(item), Mutable: true}); err != nil {
			return err
		}
		if !cur.Known[r.FullName] {
			cur.Known[r.FullName] = true
			run.Item(1)
		}
		if run.Config.Get("code") != "false" {
			if err := mirror(ctx, run, c, &cur, r, folder); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// One broken repository must not stop the others.
				run.Logf("%s: %s", r.FullName, err)
				problems = append(problems, r.Name)
			}
		}
		if run.Config.Get("issues") != "false" {
			if err := issues(ctx, run, c, &cur, r, folder); err != nil {
				return err
			}
		}
		if err := run.Save(cur); err != nil {
			return err
		}
	}
	if run.Config.Get("stars") != "false" {
		if err := stars(ctx, run, c, user); err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("no se pudo copiar el código de: %s (mira el registro)", strings.Join(problems, ", "))
	}
	return nil
}

// ---- Code ----

func (c *client) gitEnv(cloneURL string) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if c.token == "" {
		return env
	}
	// The token goes in the environment, not in the command line, where any
	// user of the machine could read it; and only to the host of the account.
	u, err := url.Parse(cloneURL)
	api, _ := url.Parse(c.api)
	if err != nil || u.Scheme != "https" || api == nil || !(u.Host == strings.TrimPrefix(api.Host, "api.") || u.Host == api.Host) {
		return env
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + c.token))
	return append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
}

func git(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(lastLine(stderr.String())))
	}
	return out, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func mirror(ctx context.Context, run *plugin.Run, c *client, cur *cursor, r repo, folder string) error {
	dir := filepath.Join(run.CacheDir, "git", plugin.SafeName(r.Owner.Login, 60)+"__"+plugin.SafeName(r.Name, 100)+".git")
	env := c.gitEnv(r.CloneURL)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		os.RemoveAll(dir)
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return err
		}
		run.Logf("Clonando %s", r.FullName)
		if _, err := git(ctx, env, "clone", "--mirror", "--quiet", r.CloneURL, dir); err != nil {
			os.RemoveAll(dir)
			return err
		}
	} else if _, err := git(ctx, env, "-C", dir, "remote", "update", "--prune"); err != nil {
		return err
	}
	refs, err := git(ctx, env, "-C", dir, "show-ref")
	if len(bytes.TrimSpace(refs)) == 0 {
		return nil // an empty repository: nothing to keep yet
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(refs)
	fingerprint := hex.EncodeToString(sum[:])
	if cur.Refs[r.FullName] == fingerprint {
		return nil
	}
	// Absolute: git runs with -C inside the mirror, where a relative path would
	// point inside the repository.
	bundle, err := filepath.Abs(filepath.Join(run.CacheDir, "git", plugin.SafeName(r.Name, 100)+".bundle"))
	if err != nil {
		return err
	}
	defer os.Remove(bundle)
	if _, err := git(ctx, env, "-C", dir, "bundle", "create", bundle, "--all"); err != nil {
		return err
	}
	info, err := os.Stat(bundle)
	if err != nil {
		return err
	}
	run.Logf("Enviando %s (%d KB)", filepath.Base(bundle), info.Size()/1024)
	if _, err := run.Out.Write(ctx, plugin.File{Path: folder + "/" + plugin.SafeName(r.Name, 100) + ".bundle", ModTime: run.Now(), Size: info.Size(),
		Open: func() (io.ReadCloser, error) { return os.Open(bundle) }, Mutable: true}); err != nil {
		return err
	}
	cur.Refs[r.FullName] = fingerprint
	return nil
}

// ---- Issues and pull requests ----

type issue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Comments  int    `json:"comments"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type comment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

func issues(ctx context.Context, run *plugin.Run, c *client, cur *cursor, r repo, folder string) error {
	uri := fmt.Sprintf("/repos/%s/%s/issues?state=all&sort=updated&direction=asc&per_page=100", url.PathEscape(r.Owner.Login), url.PathEscape(r.Name))
	since := cur.IssuesSince[r.FullName]
	if since != "" {
		uri += "&since=" + url.QueryEscape(since)
	}
	for uri != "" {
		var page []json.RawMessage
		next, err := c.get(ctx, uri, &page)
		if errors.Is(err, errGone) || errors.Is(err, plugin.ErrNotExist) {
			return nil // issues are disabled in this repository
		}
		if err != nil {
			return err
		}
		for _, raw := range page {
			var is issue
			if json.Unmarshal(raw, &is) != nil || is.Number == 0 {
				continue
			}
			comments := []json.RawMessage{}
			if is.Comments > 0 {
				commentsURI := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", url.PathEscape(r.Owner.Login), url.PathEscape(r.Name), is.Number)
				if comments, err = all[json.RawMessage](ctx, c, commentsURI); err != nil {
					return err
				}
			}
			modTime, _ := time.Parse(time.RFC3339, is.UpdatedAt)
			base := fmt.Sprintf("%s/issues/%05d", folder, is.Number)
			doc := map[string]any{"issue": raw, "comments": comments}
			if _, err := run.Out.Write(ctx, plugin.File{Path: base + ".json", ModTime: modTime, Data: plugin.JSON(doc), Mutable: true}); err != nil {
				return err
			}
			if _, err := run.Out.Write(ctx, plugin.File{Path: base + ".md", ModTime: modTime, Data: issueMarkdown(is, comments), Mutable: true}); err != nil {
				return err
			}
			if since == "" || is.CreatedAt > since {
				run.Item(1)
			}
			if is.UpdatedAt > cur.IssuesSince[r.FullName] {
				cur.IssuesSince[r.FullName] = is.UpdatedAt
			}
		}
		uri = next
	}
	return nil
}

func issueMarkdown(is issue, comments []json.RawMessage) []byte {
	var b bytes.Buffer
	kind := "Issue"
	if len(is.PullRequest) > 0 && string(is.PullRequest) != "null" {
		kind = "Pull request"
	}
	fmt.Fprintf(&b, "# %s #%d: %s\n\n", kind, is.Number, strings.TrimSpace(is.Title))
	fmt.Fprintf(&b, "- Estado: %s\n- Abierta por: %s, %s\n- Original: <%s>\n\n", is.State, is.User.Login, is.CreatedAt, is.HTMLURL)
	b.WriteString(strings.TrimSpace(is.Body))
	b.WriteString("\n")
	for _, raw := range comments {
		var cm comment
		json.Unmarshal(raw, &cm)
		fmt.Fprintf(&b, "\n---\n\n**%s** · %s\n\n%s\n", cm.User.Login, cm.CreatedAt, strings.TrimSpace(cm.Body))
	}
	return b.Bytes()
}

// ---- Stars ----

type star struct {
	FullName    string `json:"full_name"`
	HTMLURL     string `json:"html_url"`
	Description string `json:"description"`
	Language    string `json:"language"`
}

func stars(ctx context.Context, run *plugin.Run, c *client, user string) error {
	list, err := all[star](ctx, c, "/users/"+url.PathEscape(user)+"/starred?per_page=100")
	if err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].FullName) < strings.ToLower(list[j].FullName) })
	var md bytes.Buffer
	fmt.Fprintf(&md, "# Estrellas de %s\n\n", user)
	for _, s := range list {
		fmt.Fprintf(&md, "- [%s](%s)", s.FullName, s.HTMLURL)
		if s.Description != "" {
			fmt.Fprintf(&md, ": %s", strings.Join(strings.Fields(s.Description), " "))
		}
		md.WriteString("\n")
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: "estrellas.json", ModTime: run.Now(), Data: plugin.JSON(list), Mutable: true}); err != nil {
		return err
	}
	_, err = run.Out.Write(ctx, plugin.File{Path: "estrellas.md", ModTime: run.Now(), Data: md.Bytes(), Mutable: true})
	return err
}
