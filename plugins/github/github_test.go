package github_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/github"
	"github.com/fulldump/datame-suitcase/plugintest"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Prueba", "-c", "user.email=p@example.org", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return string(out)
}

func commit(t *testing.T, dir, file, content string) {
	biff.AssertNil(os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644))
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "cambio "+file)
}

// fakeGitHub answers the few API routes the integration uses, with a local
// repository as clone URL.
type fakeGitHub struct {
	mutex    sync.Mutex
	repoPath string
	issues   []map[string]any
	requests []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.requests = append(f.requests, r.URL.RequestURI())
	auth := r.Header.Get("Authorization")
	write := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/users/ana":
		write(map[string]any{"login": "ana"})
	case r.URL.Path == "/user":
		if auth != "Bearer secreto" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		write(map[string]any{"login": "ana"})
	case r.URL.Path == "/user/repos" && r.URL.Query().Get("page") == "":
		// Two pages, to follow the Link header.
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/user/repos?page=2>; rel="next"`, r.Host))
		write([]map[string]any{{"name": "proyecto", "full_name": "ana/proyecto", "clone_url": f.repoPath, "owner": map[string]any{"login": "ana"}, "private": true}})
	case r.URL.Path == "/user/repos":
		write([]map[string]any{{"name": "fork", "full_name": "ana/fork", "clone_url": "/no/existe", "fork": true, "owner": map[string]any{"login": "ana"}}})
	case r.URL.Path == "/repos/ana/proyecto/issues":
		since := r.URL.Query().Get("since")
		out := []map[string]any{}
		for _, is := range f.issues {
			if is["updated_at"].(string) >= since {
				out = append(out, is)
			}
		}
		write(out)
	case r.URL.Path == "/repos/ana/proyecto/issues/1/comments":
		write([]map[string]any{{"body": "Un comentario", "created_at": "2025-01-02T00:00:00Z", "user": map[string]any{"login": "beto"}}})
	case r.URL.Path == "/users/ana/starred":
		write([]map[string]any{{"full_name": "fulldump/box", "html_url": "https://github.com/fulldump/box", "description": "HTTP router"}})
	default:
		http.NotFound(w, r)
	}
}

func TestGitHubMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	source := t.TempDir()
	run(t, source, "init", "-q")
	commit(t, source, "README.md", "hola\n")

	fake := &fakeGitHub{repoPath: source, issues: []map[string]any{
		{"number": 1, "title": "Falla algo", "state": "open", "body": "Descripción", "html_url": "https://github.com/ana/proyecto/issues/1",
			"created_at": "2025-01-01T00:00:00Z", "updated_at": "2025-01-02T00:00:00Z", "comments": 1, "user": map[string]any{"login": "ana"}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()

	h := plugintest.New(t, github.Plugin{}, plugin.Config{"user": "ana", "token": "secreto", "api": server.URL})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2) // the repository and its issue; the fork is left out

	bundle := filepath.Join(t.TempDir(), "proyecto.bundle")
	biff.AssertNil(os.WriteFile(bundle, h.File("repos/proyecto/proyecto.bundle"), 0o644))
	restored := filepath.Join(t.TempDir(), "restaurado")
	run(t, filepath.Dir(restored), "clone", "-q", bundle, restored)
	data, err := os.ReadFile(filepath.Join(restored, "README.md"))
	biff.AssertNil(err)
	biff.AssertEqual(string(data), "hola\n")

	md := string(h.File("repos/proyecto/issues/00001.md"))
	biff.AssertTrue(strings.Contains(md, "# Issue #1: Falla algo"))
	biff.AssertTrue(strings.Contains(md, "Un comentario"))
	biff.AssertTrue(strings.Contains(string(h.File("estrellas.md")), "[fulldump/box](https://github.com/fulldump/box): HTTP router"))
	biff.AssertTrue(h.Has("repos/proyecto/repo.json"))
	biff.AssertFalse(h.Has("repos/fork/repo.json"))

	// Nothing changed: no bundle is made or sent again.
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A new commit: the bundle is replaced (the NAS keeps the previous one) and
	// has the new commit.
	commit(t, source, "b.txt", "b\n")
	r = h.Run()
	biff.AssertNil(r.Err)
	file, _ := h.NAS.File("principal", "/Archivo/repos/proyecto/proyecto.bundle")
	biff.AssertEqual(file.Versions, 2)
	biff.AssertNil(os.WriteFile(bundle, file.Data, 0o644))
	out := run(t, source, "bundle", "list-heads", bundle)
	head := strings.TrimSpace(run(t, source, "rev-parse", "HEAD"))
	biff.AssertTrue(strings.Contains(out, head))

	// The issues are asked from the last one seen.
	fake.mutex.Lock()
	found := false
	for _, req := range fake.requests {
		if strings.Contains(req, "since=2025-01-02") {
			found = true
		}
	}
	fake.mutex.Unlock()
	biff.AssertTrue(found)
}

func TestGitHubValidate(t *testing.T) {
	server := httptest.NewServer(&fakeGitHub{})
	defer server.Close()
	h := plugintest.New(t, github.Plugin{}, plugin.Config{"user": "nadie", "api": server.URL, "code": "false"})
	biff.AssertNotNil(h.Validate())
	h.Config = plugin.Config{"user": "ana", "token": "malo", "api": server.URL, "code": "false"}
	biff.AssertNotNil(h.Validate())
}

// The data folder given as a relative path, as in «-dir suitcase»: git runs
// inside the mirror, and the bundle must still be made where it is expected.
func TestGitHubMirrorWithARelativeCache(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	source := t.TempDir()
	run(t, source, "init", "-q")
	commit(t, source, "README.md", "hola\n")
	server := httptest.NewServer(&fakeGitHub{repoPath: source})
	defer server.Close()

	work := t.TempDir()
	t.Chdir(work)
	h := plugintest.New(t, github.Plugin{}, plugin.Config{"user": "ana", "token": "secreto", "api": server.URL, "issues": "false", "stars": "false"})
	biff.AssertNil(os.Mkdir("cache", 0o700))
	h.CacheDir = "cache"
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertTrue(h.Has("repos/proyecto/proyecto.bundle"))
}
