package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxWait is the longest the client sleeps for the rate limit to reset
// (Mastodon counts requests in windows of 5 minutes). A
// longer wait ends the run instead: what was fetched is written, the cursor
// says where to go on, and the engine tries again within the hour.
var maxWait = 5*time.Minute + 10*time.Second

// maxMedia caps one downloaded attachment. Mastodon itself limits videos to
// about 100 MB; this only protects against a broken server.
const maxMedia = 1 << 30

// errRateLimited ends a run politely: the next one goes on.
var errRateLimited = errors.New("el servidor ha pedido esperar (límite de peticiones); seguirá en la próxima ejecución")

type client struct {
	http     *http.Client
	instance *url.URL
	token    string

	// What the last response said about the rate limit.
	remaining int
	reset     time.Time
}

// normalizeInstance accepts «mastodon.social», «https://mastodon.social/» or
// a profile URL, and returns the base URL of the server.
func normalizeInstance(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("falta la dirección del servidor")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("«%s» no parece la dirección de un servidor", raw)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

func newClient(httpClient *http.Client, instance, token string) (*client, error) {
	u, err := normalizeInstance(instance)
	if err != nil {
		return nil, err
	}
	return &client{http: httpClient, instance: u, token: strings.TrimSpace(token), remaining: -1}, nil
}

// waitForLimit sleeps when the server said there are no requests left, if
// the reset is near; otherwise it stops the run.
func (c *client) waitForLimit(ctx context.Context, reset time.Time) error {
	wait := time.Until(reset)
	if wait <= 0 {
		return nil
	}
	if wait > maxWait {
		return errRateLimited
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *client) readLimit(h http.Header) {
	if v, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil {
		c.remaining = v
	}
	if t, err := time.Parse(time.RFC3339Nano, h.Get("X-RateLimit-Reset")); err == nil {
		c.reset = t
	}
}

// resetOf says when a 429 can be retried: Retry-After, or the reset header.
func resetOf(h http.Header, fallback time.Time) time.Time {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return time.Now().Add(time.Duration(s) * time.Second)
	}
	if t, err := http.ParseTime(h.Get("Retry-After")); err == nil {
		return t
	}
	if !fallback.IsZero() {
		return fallback
	}
	return time.Now().Add(time.Hour)
}

// do sends a GET to the API and returns the response with a 200 status.
func (c *client) do(ctx context.Context, uri string) (*http.Response, error) {
	if !strings.HasPrefix(uri, "http") {
		uri = c.instance.String() + uri
	}
	// Page links come from the server's Link header: never follow one to
	// another host, which would get the token.
	if u, err := url.Parse(uri); err != nil || u.Host != c.instance.Host {
		return nil, fmt.Errorf("enlace de página a otro servidor: %q", uri)
	}
	for attempt := 0; ; attempt++ {
		if c.remaining == 0 {
			if err := c.waitForLimit(ctx, c.reset); err != nil {
				return nil, err
			}
			c.remaining = -1
		}
		req, err := http.NewRequestWithContext(ctx, "GET", uri, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("no se pudo hablar con %s: %w", c.instance.Host, err)
		}
		c.readLimit(resp.Header)
		switch resp.StatusCode {
		case http.StatusOK:
			return resp, nil
		case http.StatusTooManyRequests:
			resp.Body.Close()
			if attempt > 0 {
				return nil, errRateLimited
			}
			if err := c.waitForLimit(ctx, resetOf(resp.Header, c.reset)); err != nil {
				return nil, err
			}
			c.remaining = -1
			continue
		case http.StatusUnauthorized:
			resp.Body.Close()
			return nil, errors.New("el servidor no acepta el token de acceso: puede que la aplicación se haya borrado o el token esté mal copiado")
		case http.StatusForbidden:
			resp.Body.Close()
			return nil, errors.New("el token no tiene permiso para esto: revisa que tenga los permisos de lectura (read:…)")
		case http.StatusNotFound:
			resp.Body.Close()
			return nil, errNotFound
		default:
			message, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			resp.Body.Close()
			return nil, fmt.Errorf("%s respondió %d: %s", c.instance.Host, resp.StatusCode, strings.TrimSpace(string(message)))
		}
	}
}

var errNotFound = errors.New("no existe")

// get reads one page of JSON and returns the links to the next (older) and
// previous (newer) pages, from the Link header.
func (c *client) get(ctx context.Context, uri string, value any) (next string, err error) {
	resp, err := c.do(ctx, uri)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(value); err != nil {
		return "", fmt.Errorf("respuesta no válida de %s: %w", c.instance.Host, err)
	}
	return linkRel(resp.Header.Get("Link"), "next"), nil
}

var linkPart = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="([^"]+)"`)

func linkRel(header, rel string) string {
	for _, m := range linkPart.FindAllStringSubmatch(header, -1) {
		if m[2] == rel {
			return m[1]
		}
	}
	return ""
}

// download saves a file (an attachment, the avatar) into dir and returns its
// path. The token goes only to the server itself, never to a media CDN.
func (c *client) download(ctx context.Context, dir, src string) (string, time.Time, error) {
	u, err := url.Parse(src)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", time.Time{}, fmt.Errorf("dirección no válida: %q", src)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", src, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	if u.Host == c.instance.Host && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("respondió %d", resp.StatusCode)
	}
	f, err := os.CreateTemp(dir, "media-*")
	if err != nil {
		return "", time.Time{}, err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxMedia+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxMedia {
		err = errors.New("es demasiado grande")
	}
	if err != nil {
		os.Remove(f.Name())
		return "", time.Time{}, err
	}
	modTime, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return f.Name(), modTime, nil
}
