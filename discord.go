package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Discord Rich Presence: the launcher talks to the Discord desktop app on this computer over its
// local IPC channel and tells it what is being played. Nothing is sent anywhere else.

// builtinDiscordAppID can be set at build time: -ldflags "-X main.builtinDiscordAppID=<application id>"
var builtinDiscordAppID = ""

// sideFile reads a one-line text file placed next to the exe (used to share IDs with the group).
func sideFile(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func discordAppID(cfg *Config) string {
	if id := strings.TrimSpace(cfg.DiscordAppID); id != "" {
		return id
	}
	if id := sideFile("discord_app_id.txt"); id != "" {
		return id
	}
	return builtinDiscordAppID
}

type discordConn struct {
	rw    io.ReadWriteCloser
	appID string
	nonce int
}

func (c *discordConn) send(op uint32, payload any) error {
	body, _ := json.Marshal(payload)
	buf := make([]byte, 8+len(body))
	binary.LittleEndian.PutUint32(buf[0:], op)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(body)))
	copy(buf[8:], body)
	_, err := c.rw.Write(buf)
	return err
}

func (c *discordConn) recv() (M, error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(c.rw, head); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(head[4:])
	if n > 1<<20 {
		return nil, errors.New("oversized reply from Discord")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.rw, body); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(head[0:]) == 2 { // close frame
		return nil, fmt.Errorf("Discord closed the connection: %s", body)
	}
	var out M
	json.Unmarshal(body, &out)
	return out, nil
}

// withTimeout guards against a Discord that accepts the connection but never answers.
func withTimeout(d time.Duration, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return errors.New("Discord did not answer")
	}
}

func discordConnect(appID string) (*discordConn, error) {
	var last error = errors.New("Discord is not running")
	for i := 0; i < 10; i++ {
		rw, err := dialDiscord(i)
		if err != nil {
			continue
		}
		c := &discordConn{rw: rw, appID: appID}
		err = withTimeout(5*time.Second, func() error {
			if err := c.send(0, M{"v": 1, "client_id": appID}); err != nil {
				return err
			}
			_, err := c.recv()
			return err
		})
		if err == nil {
			return c, nil
		}
		last = err
		rw.Close()
	}
	return nil, last
}

func (c *discordConn) setActivity(activity M) error {
	c.nonce++
	args := M{"pid": os.Getpid()}
	if activity != nil {
		args["activity"] = activity
	}
	return withTimeout(5*time.Second, func() error {
		if err := c.send(1, M{"cmd": "SET_ACTIVITY", "args": args, "nonce": fmt.Sprintf("mb-%d", c.nonce)}); err != nil {
			return err
		}
		_, err := c.recv()
		return err
	})
}

// currentActivity describes what the launcher is doing right now.
func (a *App) currentActivity() M {
	a.mu.Lock()
	running, started, launched := a.gameInst, a.gameStart, a.launched
	a.mu.Unlock()
	assets := M{"large_image": "logo", "large_text": appName}
	if running != "" {
		if inst := findInstance(a.p, running); inst != nil {
			state := "Minecraft " + inst.MCVersion
			if inst.Loader != "vanilla" {
				state += " · " + strings.ToUpper(inst.Loader[:1]) + inst.Loader[1:]
			}
			if n := countMods(inst); n > 0 {
				state += fmt.Sprintf(" · %d mods", n)
			}
			return M{"details": "Playing " + inst.Name, "state": state, "timestamps": M{"start": started.Unix()}, "assets": assets}
		}
	}
	state := "Picking a pack"
	if n := len(listInstances(a.p)); n == 0 {
		state = "Setting up"
	}
	return M{"details": "In the launcher", "state": state, "timestamps": M{"start": launched.Unix()}, "assets": assets}
}

// discordLoop keeps Discord's status in step with the launcher. It is quiet when Discord is not running.
func (a *App) discordLoop() {
	var conn *discordConn
	var sent string
	var nextTry time.Time
	reported := false
	for {
		time.Sleep(2 * time.Second)
		cfg := a.configCopy()
		appID := discordAppID(&cfg)
		on := !cfg.DiscordOff && appID != ""
		a.mu.Lock()
		a.discordLive = conn != nil
		a.mu.Unlock()
		if !on || (conn != nil && conn.appID != appID) {
			if conn != nil {
				conn.setActivity(nil)
				conn.rw.Close()
				conn, sent = nil, ""
			}
			if !on {
				continue
			}
		}
		if conn == nil {
			if time.Now().Before(nextTry) {
				continue
			}
			c, err := discordConnect(appID)
			if err != nil {
				nextTry = time.Now().Add(20 * time.Second)
				if !reported && err.Error() != "Discord is not running" {
					reported = true
					a.logf("Discord status: %v (check the Discord application ID in Settings).", err)
				}
				continue
			}
			conn, sent = c, ""
			a.logf("Connected to Discord.")
		}
		activity := a.currentActivity()
		key := string(mustMarshal(activity))
		if key == sent {
			continue
		}
		if err := conn.setActivity(activity); err != nil {
			conn.rw.Close()
			conn, sent = nil, ""
			nextTry = time.Now().Add(20 * time.Second)
			continue
		}
		sent = key
	}
}

func mustMarshal(v any) []byte { b, _ := json.Marshal(v); return b }
