//go:build !windows

package gui

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// WebViewVersion reports no embedded browser: off Windows the interface
// runs in the default browser.
func WebViewVersion() string { return "" }

func RunWindow(WindowOptions) error {
	return errors.New("the application window is only available on Windows")
}

func Info(title, text string)  { fmt.Fprintf(os.Stdout, "%s\n", text) }
func Error(title, text string) { fmt.Fprintf(os.Stderr, "%s: %s\n", title, text) }

// Confirm asks on the terminal; anything but "y" or "yes" means no.
func Confirm(title, text string) bool {
	fmt.Fprintf(os.Stdout, "%s\n[y/N] ", text)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func open(target string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", target).Start()
	}
	return exec.Command("xdg-open", target).Start()
}

func OpenURL(url string) error { return open(url) }

func OpenFolder(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return open(path)
}
