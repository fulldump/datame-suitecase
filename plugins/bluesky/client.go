package bluesky

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultEntryway = "https://bsky.social"
	maxBlob         = 1 << 30
	maxCAR          = 4 << 30
)

type client struct {
	http  *http.Client
	pds   string // where the repository lives, for every call after the login
	token string
}

type session struct {
	AccessJwt string          `json:"accessJwt"`
	DID       string          `json:"did"`
	Handle    string          `json:"handle"`
	DIDDoc    json.RawMessage `json:"didDoc"`
}

// normalizeURL accepts «bsky.social» or «https://pds.example/».
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("«%s» no parece la dirección de un servidor", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// pdsFromDIDDoc finds the PDS in the DID document that createSession returns,
// which spares asking plc.directory or the handle's domain.
func pdsFromDIDDoc(doc json.RawMessage) string {
	var d struct {
		Service []struct {
			ID              string `json:"id"`
			Type            string `json:"type"`
			ServiceEndpoint any    `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if json.Unmarshal(doc, &d) != nil {
		return ""
	}
	for _, s := range d.Service {
		if strings.HasSuffix(s.ID, "#atproto_pds") || s.Type == "AtprotoPersonalDataServer" {
			if endpoint, ok := s.ServiceEndpoint.(string); ok {
				if u, err := normalizeURL(endpoint); err == nil {
					return u
				}
			}
		}
	}
	return ""
}

// login opens a session with the app password. The entryway (bsky.social, or
// the PDS given) answers for any account it hosts, and says in the DID
// document which PDS holds the repository.
func login(ctx context.Context, httpClient *http.Client, entryway, handle, password string) (*client, *session, error) {
	if entryway == "" {
		entryway = defaultEntryway
	}
	base, err := normalizeURL(entryway)
	if err != nil {
		return nil, nil, err
	}
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	body, _ := json.Marshal(map[string]string{"identifier": handle, "password": strings.TrimSpace(password)})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/xrpc/com.atproto.server.createSession", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("no se pudo hablar con %s: %w", base, err)
	}
	defer resp.Body.Close()
	if err := statusError(resp, base); err != nil {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, nil, errors.New("Bluesky no acepta el usuario o la contraseña de aplicación")
		}
		return nil, nil, err
	}
	var s session
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil || s.AccessJwt == "" || !strings.HasPrefix(s.DID, "did:") {
		return nil, nil, fmt.Errorf("%s no ha devuelto una sesión válida", base)
	}
	pds := pdsFromDIDDoc(s.DIDDoc)
	if pds == "" {
		pds = base
	}
	return &client{http: httpClient, pds: pds, token: s.AccessJwt}, &s, nil
}

// statusError turns an XRPC error into a sentence.
func statusError(resp *http.Response, host string) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var x struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	json.Unmarshal(data, &x)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		when := ""
		if s, err := strconv.ParseInt(resp.Header.Get("RateLimit-Reset"), 10, 64); err == nil {
			when = " hasta las " + time.Unix(s, 0).Format("15:04")
		}
		return fmt.Errorf("Bluesky ha pedido esperar%s (límite de peticiones); seguirá en la próxima ejecución", when)
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("la sesión de Bluesky no es válida: revisa la contraseña de aplicación")
	case x.Error == "RepoNotFound" || x.Error == "RepoTakendown" || x.Error == "RepoDeactivated" || x.Error == "RepoSuspended":
		return fmt.Errorf("el repositorio no está disponible (%s)", x.Error)
	case x.Message != "":
		return fmt.Errorf("%s respondió %d: %s", host, resp.StatusCode, x.Message)
	}
	return fmt.Errorf("%s respondió %d: %s", host, resp.StatusCode, strings.TrimSpace(string(data)))
}

func (c *client) request(ctx context.Context, method string, params url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.pds+"/xrpc/"+method+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("no se pudo hablar con %s: %w", c.pds, err)
	}
	if err := statusError(resp, c.pds); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (c *client) get(ctx context.Context, method string, params url.Values, value any) error {
	resp, err := c.request(ctx, method, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(value)
}

// download saves the body of a call into a new file of dir.
func (c *client) download(ctx context.Context, dir, method string, params url.Values, limit int64) (string, string, error) {
	resp, err := c.request(ctx, method, params)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(dir, "descarga-*")
	if err != nil {
		return "", "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = errors.New("es demasiado grande")
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), resp.Header.Get("Content-Type"), nil
}
