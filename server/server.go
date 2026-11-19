// Package server is the local web of the program: a small API over the engine
// and the page that uses it.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/fulldump/box"

	"github.com/fulldump/datame-suitcase/engine"
	"github.com/fulldump/datame-suitcase/nas"
	"github.com/fulldump/datame-suitcase/plugin"
)

//go:embed web
var webFiles embed.FS

// Options for the handler.
type Options struct {
	// Password protects the web with HTTP basic auth (any user name). Without
	// it, the web only answers to requests addressed to this same machine.
	Password string
}

// Handler returns the web and its API.
func Handler(e *engine.Engine, options Options) http.Handler {
	b := box.NewBox()
	b.HandleResourceNotFound = func(ctx context.Context) {
		writeError(box.GetResponse(ctx), http.StatusNotFound, "no existe")
	}
	b.HandleMethodNotAllowed = func(ctx context.Context) {
		writeError(box.GetResponse(ctx), http.StatusMethodNotAllowed, "método no permitido")
	}
	b.WithInterceptors(guard(options))

	b.Handle("GET", "/", serve("web/index.html", "text/html; charset=utf-8"))
	b.Handle("GET", "/app.js", serve("web/app.js", "text/javascript; charset=utf-8"))
	b.Handle("GET", "/style.css", serve("web/style.css", "text/css; charset=utf-8"))
	b.Handle("GET", "/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	api := b.Group("/api")
	api.Handle("GET", "/plugins", func(w http.ResponseWriter, r *http.Request) {
		out := []plugin.Info{}
		for _, p := range plugin.All() {
			out = append(out, p.Info())
		}
		writeJSON(w, http.StatusOK, out)
	})

	api.Handle("GET", "/nas", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, e.NAS(r.Context()))
	})
	api.Handle("POST", "/nas", func(w http.ResponseWriter, r *http.Request) {
		input := struct {
			Code string `json:"code"`
		}{}
		if !decode(w, r, &input) {
			return
		}
		view, err := e.Pair(r.Context(), input.Code)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, nas.ErrNoNAS) {
				status = http.StatusBadGateway
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
	api.Handle("DELETE", "/nas", func(w http.ResponseWriter, r *http.Request) {
		if err := e.Unpair(r.Context()); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	api.Handle("GET", "/integrations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, e.List())
	})
	api.Handle("POST", "/integrations", func(w http.ResponseWriter, r *http.Request) {
		var input engine.Input
		if !decode(w, r, &input) {
			return
		}
		view, err := e.Create(r.Context(), input)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, view)
	})
	api.Handle("GET", "/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		view, err := e.Get(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
	api.Handle("PUT", "/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		var input engine.Input
		if !decode(w, r, &input) {
			return
		}
		view, err := e.Update(r.Context(), r.PathValue("id"), input)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
	api.Handle("DELETE", "/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := e.Delete(r.Context(), r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	api.Handle("POST", "/integrations/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		started, err := e.Start(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"started": started})
	})
	api.Handle("GET", "/integrations/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		lines, err := e.Log(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, lines)
	})
	return b
}

// guard keeps other web pages and other machines out.
//
// Without a password, only requests addressed to localhost are served: this
// stops DNS rebinding (a page whose name suddenly points to 127.0.0.1). Every
// change needs the X-Suitcase header, which a page of another origin cannot
// send without a CORS preflight this server never approves.
func guard(options Options) box.I {
	return func(next box.H) box.H {
		return func(ctx context.Context) {
			w, r := box.GetResponse(ctx), box.GetRequest(ctx)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "no-referrer")
			if options.Password != "" {
				_, password, ok := r.BasicAuth()
				if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(options.Password)) != 1 {
					w.Header().Set("WWW-Authenticate", `Basic realm="datame-suitcase", charset="UTF-8"`)
					writeError(w, http.StatusUnauthorized, "hace falta la contraseña")
					return
				}
			} else if !loopbackHost(r.Host) {
				writeError(w, http.StatusForbidden, "esta web solo se abre desde la propia máquina (localhost); arranca con -password para abrirla desde otra")
				return
			}
			if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Suitcase") != "1" {
				writeError(w, http.StatusForbidden, "petición rechazada")
				return
			}
			next(ctx)
		}
	}
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func serve(name, contentType string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := webFiles.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusNotFound, "no existe")
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasPrefix(contentType, "text/html") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		}
		w.Write(data)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %s", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func fail(w http.ResponseWriter, err error) {
	var validation engine.ValidationError
	switch {
	case errors.As(err, &validation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		log.Printf("error: %s", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "petición no válida")
		return false
	}
	return true
}
