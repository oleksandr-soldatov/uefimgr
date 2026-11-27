// uefimgr is a UEFI boot entry manager for Windows with a graphical
// interface. Started without arguments it opens its window.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/oleksandr-soldatov/uefimgr/internal/demo"
	"github.com/oleksandr-soldatov/uefimgr/internal/esp"
	"github.com/oleksandr-soldatov/uefimgr/internal/gui"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
	"github.com/oleksandr-soldatov/uefimgr/internal/ui"
)

var version = "dev"

const appTitle = "uefimgr"

const usage = `uefimgr — UEFI boot entry manager

  uefimgr                    open the application window
  uefimgr restore FILE       restore a backup without the window, for use
                             from a recovery environment

Options:
  --browser                  show the interface in the default web browser
  --store file:PATH          use a simulated variable file instead of firmware
  --backup-dir DIR           where backups are kept
  --addr HOST:PORT           with --browser: the address to listen on
  --no-open                  with --browser: do not open the browser
  --version                  print the version
`

// The window and its embedded browser must stay on the main thread.
func init() { runtime.LockOSThread() }

func main() { os.Exit(run(os.Args[1:])) }

type options struct {
	store     string
	backupDir string
	browser   bool
	addr      string
	noOpen    bool
}

func run(args []string) int {
	var o options
	fs := flag.NewFlagSet(appTitle, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.store, "store", "", "")
	fs.StringVar(&o.backupDir, "backup-dir", "", "")
	fs.BoolVar(&o.browser, "browser", false, "")
	fs.StringVar(&o.addr, "addr", "127.0.0.1:0", "")
	fs.BoolVar(&o.noOpen, "no-open", false, "")
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			gui.Info(appTitle, usage)
			return 0
		}
		gui.Error(appTitle, err.Error()+"\n\n"+usage)
		return 2
	}
	if *showVersion {
		gui.Info(appTitle, appTitle+" "+version)
		return 0
	}
	if !hasFirmwareAccess(o) {
		gui.Error(appTitle, "uefimgr needs administrator rights to read and write the firmware's boot entries.\n\nRight-click it and choose \"Run as administrator\".")
		return 1
	}
	logToFile()

	rest := fs.Args()
	var err error
	switch {
	case len(rest) == 0:
		err = runInterface(o)
	case rest[0] == "restore" && len(rest) == 2:
		err = runRestore(o, rest[1])
	default:
		gui.Error(appTitle, "Unknown arguments: "+strings.Join(rest, " ")+"\n\n"+usage)
		return 2
	}
	if err != nil {
		gui.Error(appTitle, err.Error())
		return 1
	}
	return 0
}

// hasFirmwareAccess reports false when the program will use the firmware
// but is not running as administrator. The embedded manifest makes Windows
// elevate it at launch, so this only fails when that was bypassed, for
// example by a launcher that ignores the manifest.
func hasFirmwareAccess(o options) bool {
	needsFirmware := o.store == "" || o.store == "firmware"
	return runtime.GOOS != "windows" || !needsFirmware || store.IsElevated()
}

// open connects to the variable store and wraps it in a session.
func open(o options) (*manager.Session, esp.Provider, bool, error) {
	spec := o.store
	if spec == "" {
		if runtime.GOOS != "windows" {
			return nil, nil, false, fmt.Errorf("firmware variables can only be reached on Windows.\nTo try the interface here, run: uefimgr --store file:demo.json")
		}
		spec = "firmware"
	}
	if spec == "firmware" {
		fw := store.NewFirmware()
		if err := fw.Prepare(); err != nil {
			return nil, nil, false, err
		}
		dir := o.backupDir
		if dir == "" {
			dir = defaultBackupDir()
		}
		return manager.NewSession(fw, dir), esp.Default(), false, nil
	}
	path, ok := strings.CutPrefix(spec, "file:")
	if !ok || path == "" {
		return nil, nil, false, fmt.Errorf("unknown store %q: use firmware or file:PATH", spec)
	}
	file, err := store.OpenFile(path)
	if err != nil {
		return nil, nil, false, err
	}
	if names, _ := file.Names(); len(names) == 0 {
		if err := demo.Seed(file); err != nil {
			return nil, nil, false, err
		}
	}
	dir := o.backupDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(path), "uefimgr-backups")
	}
	return manager.NewSession(file, dir), esp.NewSimulated(), true, nil
}

func runInterface(o options) error {
	session, volumes, simulated, err := open(o)
	if err != nil {
		return err
	}
	api := ui.NewAPI(session, volumes, version)
	api.Simulated = simulated
	api.OpenFolder = gui.OpenFolder

	if !o.browser {
		if gui.WebViewVersion() != "" {
			return gui.RunWindow(gui.WindowOptions{
				Title:   appTitle,
				HTML:    ui.IndexHTML,
				Handler: api.Call,
				DataDir: filepath.Join(dataDir(), "webview"),
				Width:   1180,
				Height:  860,
				Debug:   os.Getenv("UEFIMGR_DEBUG") != "",
			})
		}
		if runtime.GOOS == "windows" {
			gui.Info(appTitle, "The Microsoft Edge WebView2 runtime is not installed, so uefimgr will open in your web browser instead.\n\nIt keeps running until that page has been closed for a few minutes.")
		}
	}
	return serve(api, o)
}

// serve runs the interface in a web browser.
func serve(api *ui.API, o options) error {
	srv, err := ui.NewServer(api)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String() + "/?token=" + srv.Token()
	fmt.Printf("%s %s\nopen: %s\n", appTitle, version, url)
	if !o.noOpen {
		if err := gui.OpenURL(url); err != nil {
			fmt.Fprintf(os.Stderr, "could not open a browser: %v\n", err)
		}
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// On Windows there is no console to stop the process from, so it ends
	// once the page has stopped calling in.
	if runtime.GOOS == "windows" {
		const idle = 3 * time.Minute
		go func() {
			for range time.Tick(15 * time.Second) {
				if time.Since(srv.LastSeen()) > idle {
					httpSrv.Close()
					return
				}
			}
		}()
	}
	if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runRestore writes a backup back with nothing but message boxes, for the
// case where the window cannot be used.
func runRestore(o options, path string) error {
	session, _, _, err := open(o)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := session.LoadSnapshot(raw); err != nil {
		return err
	}
	plan, err := session.Plan()
	if err != nil {
		return err
	}
	if len(plan.Writes) == 0 {
		gui.Info(appTitle, "The boot configuration already matches "+path+". Nothing to do.")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Restore the boot configuration from %s?\n\nThis will write:\n", path)
	for _, w := range plan.Writes {
		fmt.Fprintf(&b, "  %s %s", w.Op, w.Name)
		if w.Detail != "" {
			fmt.Fprintf(&b, "  (%s)", w.Detail)
		}
		b.WriteString("\n")
	}
	if len(plan.Risks) > 0 {
		b.WriteString("\nWARNING — this could stop the system from starting:\n")
		for _, r := range plan.Risks {
			fmt.Fprintf(&b, "  • %s\n", r.Message)
		}
	}
	if !gui.Confirm(appTitle, b.String()) {
		return nil
	}
	res, err := session.Apply(true)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Restored: %d variables written.", res.Written)
	if res.Backup != "" {
		msg += "\n\nThe previous configuration was saved to " + res.Backup
	}
	for _, w := range res.Warnings {
		msg += "\n\nWarning: " + w
	}
	gui.Info(appTitle, msg)
	return nil
}

func defaultBackupDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "uefimgr", "backups")
		}
	}
	return filepath.Join(dataDir(), "backups")
}

// dataDir is where the application keeps its own files.
func dataDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "uefimgr")
}

// logToFile sends log output, including what the embedded browser library
// reports, to a file: a windowed program has no console to print to.
func logToFile() {
	if runtime.GOOS != "windows" {
		return
	}
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "uefimgr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(f)
	log.Printf("%s %s starting", appTitle, version)
}
