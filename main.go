package main

// wlterm: a tiling Wayland compositor that lives inside a terminal pane.
// Clients render via wl_shm; wlterm tiles them, draws a tuios-shaped frame
// around them, and writes the result to stdout as kitty graphics.
//
//	wlterm [flags] -- CMD...
//	  -exec CMD     launch another client at startup (repeatable)
//	  -spawn CMD    command bound to <prefix> c
//	  -mode         b64 | shm | delta   transport
//	  -layers       single | per-window granularity
//	  -prefix       compositor leader key, default ctrl+b
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
	"sync"
	"syscall"
	"time"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	mode := flag.String("mode", "auto", "kitty transport: auto|delta|shm|b64")
	layers := flag.String("layers", "per-window", "granularity: per-window|single")
	fps := flag.Int("fps", 60, "max frames per second")
	logPath := flag.String("log", "", "debug log file")
	pixels := flag.String("pixels", "", "WxH: headless mode, no tty setup")
	cellSize := flag.String("cell", "10x20", "headless cell size WxH")
	socketName := flag.String("socket", "", "wayland socket name (default wlterm-PID)")
	stampPath := flag.String("stamp", "", "write per-frame wallclock ms to this file")
	prefix := flag.String("prefix", "ctrl+b", "compositor leader key")
	spawnCmd := flag.String("spawn", "foot", "command bound to <prefix> c")
	snapDir := flag.String("snapshots", "", "write composited PNGs into this directory")
	snapEvery := flag.Duration("snapshot-every", 200*time.Millisecond, "snapshot interval")
	var execs stringList
	flag.Var(&execs, "exec", "extra client to launch at startup (repeatable)")
	flag.Parse()

	if *logPath != "" {
		if f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			logFile = f
		}
	}

	initPalette()

	comp := &compositor{
		clients:     map[*client]bool{},
		renderCh:    make(chan struct{}, 1),
		nextImgID:   100,
		masterRatio: masterRatio,
		spawnCmd:    []string{"/bin/sh", "-c", *spawnCmd},
		prefixName:  *prefix,
		swallow:     map[uint32]bool{},
	}
	if code, mods, ok := prefixFromName(*prefix); ok {
		comp.prefixCode, comp.prefixMods = code, mods
	} else {
		fmt.Fprintf(os.Stderr, "unparsable -prefix %q\n", *prefix)
		os.Exit(2)
	}

	p := newInputParser(comp)

	headless := *pixels != ""
	if headless {
		fmt.Sscanf(*pixels, "%dx%d", &comp.widthPx, &comp.heightPx)
		fmt.Sscanf(*cellSize, "%dx%d", &comp.cellW, &comp.cellH)
		if comp.widthPx == 0 || comp.cellW == 0 {
			fmt.Fprintln(os.Stderr, "bad -pixels or -cell")
			os.Exit(2)
		}
		p.kittyKB = true
		p.pixelMouse = true
		p.headless = true
		go p.run()
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
	logf("mode=%s layers=%s size=%dx%d cell=%dx%d", *mode, *layers, comp.widthPx, comp.heightPx, comp.cellW, comp.cellH)
	logf("chrome: %s", reportContrast())

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

	// Several clients now share one socket; each gets its own goroutine and
	// its own object table.
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
			logf("client connected (%d total)", len(comp.clients))
			go c.readLoop()
		}
	}()

	rend := newRenderer(comp, *mode, *layers == "per-window", os.Stdout, *fps)
	if *stampPath != "" {
		if f, err := os.Create(*stampPath); err == nil {
			rend.stamp = f
		}
	}
	go rend.loop()
	if *snapDir != "" {
		go snapshotLoop(comp, *snapDir, *snapEvery)
	}

	// Child processes. Every one of them gets OUR display, never the host's.
	childEnv := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "WAYLAND_DISPLAY=") || strings.HasPrefix(e, "DISPLAY=") {
			continue
		}
		childEnv = append(childEnv, e)
	}
	childEnv = append(childEnv, "WAYLAND_DISPLAY="+sockName)

	var childMu sync.Mutex
	children := map[int]*exec.Cmd{}
	childExit := make(chan int, 8)
	started := 0

	launch := func(argv []string) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = childEnv
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if logFile != nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
		if err := cmd.Start(); err != nil {
			logf("spawn %v failed: %v", argv, err)
			return
		}
		pid := cmd.Process.Pid
		childMu.Lock()
		children[pid] = cmd
		started++
		childMu.Unlock()
		logf("spawned %v pid=%d", argv, pid)
		go func() {
			err := cmd.Wait()
			logf("child %d exited: %v", pid, err)
			childExit <- pid
		}()
	}
	comp.spawn = func(argv []string) { go launch(argv) }

	quitOnce := sync.Once{}
	quitCh := make(chan struct{})
	comp.quitFn = func() { quitOnce.Do(func() { close(quitCh) }) }

	for _, e := range execs {
		launch([]string{"/bin/sh", "-c", e})
	}
	if args := flag.Args(); len(args) > 0 {
		launch(args)
	}
	if started == 0 {
		logf("no initial client; use %s c", *prefix)
	}

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)

	statsT := time.NewTicker(time.Second)
	defer statsT.Stop()
	statsAt := time.Now()

	killAll := func() {
		childMu.Lock()
		for pid := range children {
			syscall.Kill(-pid, syscall.SIGTERM)
		}
		childMu.Unlock()
	}

	for {
		select {
		case pid := <-childExit:
			childMu.Lock()
			delete(children, pid)
			left := len(children)
			childMu.Unlock()
			if left == 0 && started > 0 {
				time.Sleep(50 * time.Millisecond) // let the last frame drain
				cleanup(headless)
				return
			}
		case <-p.quit:
			logf("quit requested (terminal)")
			killAll()
			cleanup(headless)
			return
		case <-quitCh:
			logf("quit requested (binding)")
			killAll()
			time.Sleep(80 * time.Millisecond)
			cleanup(headless)
			return
		case s := <-sigCh:
			if s == syscall.SIGWINCH && !headless {
				os.Stdout.WriteString("\x1b[16t\x1b[14t")
				go func() {
					time.Sleep(150 * time.Millisecond)
					comp.mu.Lock()
					computeSize(comp, p)
					comp.relayout()
					comp.mu.Unlock()
				}()
				continue
			}
			killAll()
			cleanup(headless)
			return
		case <-statsT.C:
			if line := statsLine(time.Since(statsAt)); line != "" {
				comp.mu.Lock()
				n := len(comp.windows)
				comp.mu.Unlock()
				logf("stats: surfaces=%d %s", n, line)
			}
			statsAt = time.Now()
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
