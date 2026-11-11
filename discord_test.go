//go:build !windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// A stand-in Discord app: checks the launcher speaks Discord's local IPC framing correctly.
func TestDiscordPresence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "discord-ipc-0"))
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	got := make(chan M, 4)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for {
			head := make([]byte, 8)
			if _, err := io.ReadFull(c, head); err != nil {
				return
			}
			body := make([]byte, binary.LittleEndian.Uint32(head[4:]))
			io.ReadFull(c, body)
			var m M
			json.Unmarshal(body, &m)
			m["_op"] = float64(binary.LittleEndian.Uint32(head[0:]))
			got <- m
			reply := []byte(`{"cmd":"DISPATCH","evt":"READY"}`)
			out := make([]byte, 8+len(reply))
			binary.LittleEndian.PutUint32(out[0:], 1)
			binary.LittleEndian.PutUint32(out[4:], uint32(len(reply)))
			copy(out[8:], reply)
			c.Write(out)
		}
	}()

	p := newPaths(filepath.Join(dir, "data"))
	app := newApp(p, "tok")
	inst, _ := createInstance(p, "Jungle Tech", "1.20.1", "forge", "47.3.0")
	conn, err := discordConnect("123456")
	if err != nil {
		t.Fatal(err)
	}
	hs := <-got
	if asInt(hs["_op"]) != 0 || asS(hs["client_id"]) != "123456" || asInt(hs["v"]) != 1 {
		t.Fatalf("bad handshake: %v", hs)
	}
	idle := app.currentActivity()
	if asS(idle["details"]) != "In the launcher" {
		t.Fatalf("idle activity: %v", idle)
	}
	app.gameInst, app.gameStart = inst.ID, time.Unix(1700000000, 0)
	if err := conn.setActivity(app.currentActivity()); err != nil {
		t.Fatal(err)
	}
	f := <-got
	act := asM(asM(f["args"])["activity"])
	if asInt(f["_op"]) != 1 || asS(f["cmd"]) != "SET_ACTIVITY" || asS(act["details"]) != "Playing Jungle Tech" ||
		asS(act["state"]) != "Minecraft 1.20.1 · Forge" || asInt(asM(act["timestamps"])["start"]) != 1700000000 || asInt(asM(f["args"])["pid"]) == 0 {
		t.Fatalf("bad activity frame: %v", f)
	}
	if err := conn.setActivity(nil); err != nil {
		t.Fatal(err)
	}
	if f = <-got; asM(f["args"])["activity"] != nil {
		t.Fatalf("clearing should send no activity: %v", f)
	}
}
