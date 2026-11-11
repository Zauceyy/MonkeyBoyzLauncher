package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type serverInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func main() {
	dataDir := flag.String("data-dir", "", "where instances and game files are stored")
	noWindow := flag.Bool("no-window", false, "start without opening a window (prints the address)")
	port := flag.Int("port", 0, "port for the local interface (0 = pick one)")
	flag.Parse()

	p := newPaths(*dataDir)
	infoPath := filepath.Join(p.Root, "server.json")
	var pendingZip string
	if arg := flag.Arg(0); strings.HasSuffix(strings.ToLower(arg), ".zip") && exists(arg) {
		pendingZip, _ = filepath.Abs(arg) // a pack zip dropped onto the exe
	}

	// If the launcher is already running, just show its window again.
	var old serverInfo
	if readJSONInto(infoPath, &old) == nil && old.Port > 0 && pendingZip == "" && !*noWindow {
		c := http.Client{Timeout: 2 * time.Second}
		if resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state?t=%s", old.Port, old.Token)); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				openWindow(p, fmt.Sprintf("http://127.0.0.1:%d/?t=%s", old.Port, old.Token))
				return
			}
		}
	}

	tok := make([]byte, 16)
	rand.Read(tok)
	token := hex.EncodeToString(tok)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not start:", err)
		os.Exit(1)
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	app := newApp(p, token)
	writeJSON(infoPath, serverInfo{actual, token})
	defer os.Remove(infoPath)
	go http.Serve(ln, app.routes())
	go app.discordLoop()

	app.logf("%s %s", appName, appVersion)
	if pendingZip != "" {
		app.startImport(pendingZip, "", false)
	}
	address := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", actual, token)
	if *noWindow {
		fmt.Println(address)
		select {}
	}

	started := time.Now()
	win := openWindow(p, address)
	if win != nil {
		win.Wait()
	}
	if win == nil || time.Since(started) < 4*time.Second {
		// Opened in an ordinary browser tab (or handed to an already running browser):
		// stay alive while the page keeps checking in.
		for {
			time.Sleep(2 * time.Second)
			app.mu.Lock()
			quiet := time.Since(app.lastPoll)
			app.mu.Unlock()
			if (quiet > 90*time.Second || app.windowGone()) && app.idle() {
				return
			}
		}
	}
	for { // window closed: let a running game or download finish first, unless a new window attached
		time.Sleep(2 * time.Second)
		app.mu.Lock()
		quiet := time.Since(app.lastPoll)
		app.mu.Unlock()
		if app.idle() && quiet > 6*time.Second {
			return
		}
	}
}

// openWindow shows the interface in its own app-style window (Edge or Chrome), or a browser tab as a fallback.
func openWindow(p *Paths, address string) *exec.Cmd {
	for _, browser := range appBrowsers() {
		if !exists(browser) {
			continue
		}
		cmd := exec.Command(browser, "--app="+address, "--user-data-dir="+filepath.Join(p.Root, "window"),
			"--window-size=1200,780", "--no-first-run", "--no-default-browser-check", "--disable-background-mode",
			"--disable-extensions", "--disable-sync")
		if cmd.Start() == nil {
			return cmd
		}
	}
	openURL(address)
	return nil
}
