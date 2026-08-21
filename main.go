package main

// wlterm: a Wayland compositor that lives inside a terminal pane.
// Clients render via wl_shm; wlterm composites and writes the result to
// stdout as kitty graphics. Pure Go, no cgo, no libwayland.
//
//	wlterm [-mode delta|shm|b64] [-fps 60] [-log FILE] [-pixels WxH] -- CMD...
//
// The Wayland socket is always wlterm's own (wlterm-<pid> under
// XDG_RUNTIME_DIR); the host session's WAYLAND_DISPLAY is never inherited by
// children and never dialed by wlterm.

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	mode := flag.String("mode", "auto", "kitty transport: auto|delta|shm|b64")
	fps := flag.Int("fps", 60, "max frames per second")
	logPath := flag.String("log", "", "debug log file")
	pixels := flag.String("pixels", "", "WxH: headless mode, no tty setup")
	socketName := flag.String("socket", "", "wayland socket name (default wlterm-PID)")
	stampPath := flag.String("stamp", "", "write per-frame wallclock ms to this file")
	flag.Parse()

	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			logFile = f
		}
	}

	cmdArgs := flag.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: wlterm [flags] -- command...")
		os.Exit(2)
	}

	comp := &compositor{
		clients:  map[*client]bool{},
		renderCh: make(chan struct{}, 1),
	}
	p := newInputParser(comp)

	headless := *pixels != ""
	if headless {
		fmt.Sscanf(*pixels, "%dx%d", &comp.widthPx, &comp.heightPx)
		if comp.widthPx == 0 {
			fmt.Fprintln(os.Stderr, "bad -pixels")
			os.Exit(2)
		}
		comp.cellW, comp.cellH = 10, 20
		p.kittyKB = true
		p.pixelMouse = true
		p.headless = true
		go p.run() // still accept injected input on stdin
	} else {
		go p.run()
		setupTerminal(comp, p)
	}

	if *mode == "auto" {
		term := os.Getenv("TERM")
		switch {
		case strings.Contains(term, "kitty") || os.Getenv("KITTY_WINDOW_ID") != "":
			*mode = "delta"
		default:
			*mode = "shm"
		}
	}
	logf("mode=%s size=%dx%d", *mode, comp.widthPx, comp.heightPx)

	// Wayland socket: always our own name, never the host session's.
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = "/tmp"
	}
	sockName := *socketName
	if sockName == "" {
		sockName = fmt.Sprintf("wlterm-%d", os.Getpid())
	}
	sockPath := runtimeDir + "/" + sockName
	os.Remove(sockPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		fatal("listen: %v", err)
	}
	defer os.Remove(sockPath)

	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			c := &client{comp: comp, conn: conn, objects: map[uint32]object{}}
			c.objects[1] = wlDisplay{}
			comp.mu.Lock()
			comp.clients[c] = true
			comp.mu.Unlock()
			logf("client connected")
			go c.readLoop()
		}
	}()

	rend := newRenderer(comp, *mode, os.Stdout, *fps)
	if *stampPath != "" {
		if f, err := os.Create(*stampPath); err == nil {
			rend.stamp = f
		}
	}
	go rend.loop()

	// Child process: gets OUR display, never the host's.
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	env := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "WAYLAND_DISPLAY=") || strings.HasPrefix(e, "DISPLAY=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "WAYLAND_DISPLAY="+sockName)
	cmd.Env = env
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		if !headless {
			teardownTerminal()
		}
		fatal("start %s: %v", cmdArgs[0], err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)

	statsT := time.NewTicker(time.Second)
	defer statsT.Stop()

	for {
		select {
		case err := <-childDone:
			logf("child exited: %v", err)
			time.Sleep(50 * time.Millisecond) // let the last frame drain
			cleanup(headless)
			return
		case <-p.quit:
			logf("quit requested")
			cmd.Process.Kill()
			cleanup(headless)
			return
		case s := <-sigCh:
			if s == syscall.SIGWINCH && !headless {
				comp.mu.Lock()
				os.Stdout.WriteString("\x1b[16t\x1b[14t")
				comp.mu.Unlock()
				go func() {
					time.Sleep(150 * time.Millisecond)
					comp.mu.Lock()
					computeSize(comp, p)
					if comp.toplevel != nil {
						comp.toplevel.resize(comp)
					}
					comp.dirty = true
					comp.dirtyAt = time.Now()
					comp.mu.Unlock()
					select {
					case comp.renderCh <- struct{}{}:
					default:
					}
				}()
				continue
			}
			if s == syscall.SIGINT || s == syscall.SIGTERM {
				cmd.Process.Kill()
				cleanup(headless)
				return
			}
		case <-statsT.C:
			if line := statsLine(); line != "" {
				logf("stats: %s", line)
			}
		}
	}
}

func cleanup(headless bool) {
	cleanupShm()
	if !headless {
		teardownTerminal()
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
