// Command datame-suitcase keeps copies of what a person has in online
// services in a folder of their Datame NAS.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fulldump/goconfig"
	"github.com/fulldump/gootstrap"

	"github.com/fulldump/datame-suitcase/engine"
	_ "github.com/fulldump/datame-suitcase/plugins/all"
	"github.com/fulldump/datame-suitcase/server"
)

// Config is read from flags, environment variables or config.json (goconfig).
type Config struct {
	Dir      string `usage:"Carpeta de datos (configuración, estado y copias de trabajo)"`
	KeyFile  string `usage:"Fichero con la clave que cifra las credenciales (por defecto <dir>/clave)"`
	Listen   string `usage:"Dirección de la web local"`
	Password string `usage:"Contraseña de la web; obligatoria si escucha fuera de localhost"`
}

func defaultDir() string {
	if dir := os.Getenv("STATE_DIRECTORY"); dir != "" { // systemd StateDirectory=
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "datame-suitcase")
	}
	return "datame-suitcase"
}

func main() {
	config := Config{Dir: defaultDir(), Listen: "127.0.0.1:4141"}
	// Variables of the environment go with a prefix (SUITCASE_PASSWORD), so a
	// generic DIR or PASSWORD of the system is never taken by mistake.
	lookup := func(name string) (string, bool) { return os.LookupEnv("SUITCASE_" + name) }
	err := goconfig.Load(&config, goconfig.WithProgramName("datame-suitcase"), goconfig.WithEnvLookup(lookup))
	if err != nil && slices.ContainsFunc(os.Args[1:], func(arg string) bool { return strings.TrimLeft(arg, "-") == "help" }) {
		fmt.Println(strings.TrimPrefix(err.Error(), "Config arg error: "))
		return
	}
	if err == nil {
		err = run(config)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(config Config) error {
	host, _, err := net.SplitHostPort(config.Listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) && config.Password == "" {
		return errors.New("la web escucha fuera de esta máquina: ponle una contraseña con -password, o usa un túnel SSH (ssh -L 4141:127.0.0.1:4141 nas)")
	}

	e, err := engine.New(engine.Options{Dir: config.Dir, KeyFile: config.KeyFile})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler:           server.Handler(e, server.Options{Password: config.Password}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Printf("Datame Suitcase en http://%s\nDatos en %s\n", config.Listen, config.Dir)
	if config.Password != "" {
		fmt.Println("AVISO: la web va por HTTP sin cifrar; la contraseña viaja en claro por tu red. Mejor un túnel SSH si puedes.")
	}

	gootstrap.Run(
		func() (start, stop func() error) {
			start = func() error {
				if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			}
			stop = func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return httpServer.Shutdown(ctx)
			}
			return start, stop
		},
		func() (start, stop func() error) {
			ctx, cancel := context.WithCancel(context.Background())
			start = func() error {
				e.Loop(ctx) // a run cut here goes on from its cursor next time
				return nil
			}
			stop = func() error {
				cancel()
				return nil
			}
			return start, stop
		},
	)
	return nil
}
