package main

// wlterm: a Wayland compositor that lives inside a terminal pane. Clients
// render via wl_shm or a LINEAR dmabuf; wlterm composites them and writes
// the result to stdout as kitty graphics.
//
//	wlterm [flags] -- CMD...
//
// Two modes. The default is single-app: one toplevel filling the pane, no
// chrome of wlterm's own, no leader key, no launcher. That is the mode for
// running a Wayland program inside a multiplexer that already draws the
// border, the title and the focus ring and already owns ctrl+b.
//
//	wlterm -- foot          a Wayland terminal in a tuios pane
//
// -multi restores the tiling compositor: a BSP layout, a tuios-shaped frame
// per window, a dock, a leader key and an application launcher.
//
//	wlterm -multi -- foot
//
//	  -multi        multi-surface tiling mode (default: single app)
//	  -exec CMD     launch another client at startup (repeatable)
//	  -spawn CMD    command bound to <prefix> c        (-multi only)
//	  -mode         b64 | shm | delta   transport
//	  -layers       single | per-window granularity
//	  -prefix       leader key, default ctrl+b         (-multi only)
//	  -quit-key     escape hatch, tapped twice; "none" to intercept nothing
//	  -no-dmabuf    do not advertise zwp_linux_dmabuf_v1
//	  -drm NODE     render node to advertise as dmabuf main_device
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
	"strconv"
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
	multi := flag.Bool("multi", false, "multi-surface tiling mode: chrome, leader key, launcher")
	prefix := flag.String("prefix", "ctrl+b", "leader key (-multi only)")
	quitKey := flag.String("quit-key", "ctrl+backslash", "escape hatch, tapped twice within 700ms; \"none\" intercepts nothing")
	noDmabuf := flag.Bool("no-dmabuf", false, "do not advertise zwp_linux_dmabuf_v1")
	drmNode := flag.String("drm", "", "render node to advertise as dmabuf main_device")
	isolate := flag.Bool("isolate", true, "give clients a private runtime dir and session bus")
	spawnCmd := flag.String("spawn", "foot", "command bound to <prefix> c")
	termCmd := flag.String("term", "foot", "terminal used for Terminal=true desktop entries")
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
	sweepOrphanShm()

	comp := &compositor{
		single:      !*multi,
		clients:     map[*client]bool{},
		renderCh:    make(chan struct{}, 1),
		nextImgID:   100,
		masterRatio: masterRatio,
		swallow:     map[uint32]bool{},
	}
	// Everything below this line exists to be navigated between windows.
	// In single-app mode there is nothing to navigate, so there is no
	// leader key: prefixCode stays zero and handleKey hands every key
	// straight to the guest. This is not a nicety. wlterm's leader was
	// ctrl+b and so is tuios's, so inside a tuios pane the leader never
	// arrived and every binding it guarded was unreachable anyway.
	if *multi {
		comp.spawnCmd = []string{"/bin/sh", "-c", *spawnCmd}
		comp.prefixName = *prefix
		comp.termCmd = defaultTermCmd(*termCmd)
		if code, mods, ok := prefixFromName(*prefix); ok {
			comp.prefixCode, comp.prefixMods = code, mods
		} else {
			fmt.Fprintf(os.Stderr, "unparsable -prefix %q\n", *prefix)
			os.Exit(2)
		}
	}

	p := newInputParser(comp)
	// The escape hatch. Two taps inside 700ms, so a single press still
	// reaches the guest; ctrl+backslash because tuios does not bind it and
	// nothing types it twice in a row on purpose.
	if *quitKey != "" && *quitKey != "none" {
		code, mods, ok := prefixFromName(strings.Replace(*quitKey, "backslash", "\\", 1))
		if !ok {
			fmt.Fprintf(os.Stderr, "unparsable -quit-key %q\n", *quitKey)
			os.Exit(2)
		}
		p.quitKey, p.quitMods, p.quitName = code, mods, *quitKey
	}

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
	kind := "single-app"
	if *multi {
		kind = "multi-surface"
	}
	quitDesc := "none (the app exiting, or a signal, is the only way out)"
	if p.quitKey != 0 {
		quitDesc = p.quitName + " twice within " + quitWindow.String()
	}
	logf("%s: mode=%s layers=%s size=%dx%d cell=%dx%d leader=%s quit=%s",
		kind, *mode, *layers, comp.widthPx, comp.heightPx, comp.cellW, comp.cellH,
		orNone(comp.prefixName), quitDesc)
	if *multi {
		logf("chrome: %s", reportContrast())
		logf("chrome: %s", reportPanelContrast())
	}
	if !*noDmabuf {
		initDmabuf(*drmNode)
	} else {
		logf("dmabuf: disabled by -no-dmabuf; clients fall back to wl_shm")
	}

	// Client isolation.
	//
	// Stripping WAYLAND_DISPLAY is necessary and nowhere near sufficient.
	// Most GTK and KDE applications are launched through the session bus:
	// the launcher asks the bus to start or raise the app, the bus routes
	// that to the copy already running on the host, a window opens on the
	// host desktop, and our child exits looking like a success. A file
	// manager did exactly that during this work.
	//
	// So the child gets a private XDG_RUNTIME_DIR and, where dbus-daemon
	// exists, a private session bus. The private runtime dir matters as
	// much as the bus address: libdbus falls back to $XDG_RUNTIME_DIR/bus
	// when DBUS_SESSION_BUS_ADDRESS is unset, so unsetting the variable
	// alone would leave the host bus one autolaunch away.
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = "/tmp"
	}
	sweepOrphanRuntime(runtimeDir)
	childRuntime := runtimeDir
	if *isolate {
		childRuntime = fmt.Sprintf("%s/wlterm-rt-%d", runtimeDir, os.Getpid())
		if err := os.MkdirAll(childRuntime, 0o700); err != nil {
			fatal("private runtime dir: %v", err)
		}
		defer os.RemoveAll(childRuntime)
	}
	sockName := *socketName
	if sockName == "" {
		sockName = fmt.Sprintf("wlterm-%d", os.Getpid())
	}
	sockPath := childRuntime + "/" + sockName
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
			c.pid, c.sid = peerIdentity(conn)
			c.objects[1] = wlDisplay{}
			comp.mu.Lock()
			comp.clients[c] = true
			comp.mu.Unlock()
			logf("client connected: pid=%d sid=%d (%d total)", c.pid, c.sid, len(comp.clients))
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

	// Child processes. Every one of them gets OUR display and OUR bus,
	// never the host session's.
	busAddr, stopBus := "", func() {}
	if *isolate {
		var busPid int
		busAddr, busPid, stopBus = startPrivateBus(childRuntime, sockName)
		defer stopBus()
		comp.mu.Lock()
		comp.busSid = sessionOf(busPid)
		comp.mu.Unlock()
	}
	childEnv := []string{}
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && leakyEnv[k] {
			continue
		}
		childEnv = append(childEnv, e)
	}
	childEnv = append(childEnv, "WAYLAND_DISPLAY="+sockName)
	if *isolate {
		childEnv = append(childEnv, "XDG_RUNTIME_DIR="+childRuntime)
		if busAddr != "" {
			childEnv = append(childEnv, "DBUS_SESSION_BUS_ADDRESS="+busAddr)
		}
	}
	logf("isolation: runtime_dir=%s bus=%q", childRuntime, busAddr)

	var childMu sync.Mutex
	children := map[int]*exec.Cmd{}
	childExit := make(chan int, 8)
	started := 0

	launch := func(req spawnReq) {
		argv := req.argv
		if len(argv) == 0 {
			return
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = childEnv
		// Path= from a desktop entry is the working directory. A bad one
		// is the entry's problem, not a reason to refuse the launch.
		if req.dir != "" {
			if st, err := os.Stat(req.dir); err == nil && st.IsDir() {
				cmd.Dir = req.dir
			} else {
				logf("ignoring Path=%q for %s: %v", req.dir, req.name(), err)
			}
		}
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
		logf("spawned %q pid=%d argv=%q", req.name(), pid, argv)
		startedAt := time.Now()
		// Setsid above makes the child a session leader, so its session id
		// equals its pid and every descendant inherits it. That is the
		// handle used to verify the launch below.
		go verifyLaunch(comp, req, pid, startedAt)
		go func() {
			err := cmd.Wait()
			lived := time.Since(startedAt)
			comp.mu.Lock()
			mapped := false
			for _, w := range comp.windows {
				if w.top == nil || w.top.client == nil {
					continue
				}
				if w.top.client.sid == pid ||
					(comp.busSid != 0 && w.top.client.sid == comp.busSid) {
					mapped = true
				}
			}
			comp.mu.Unlock()
			// A GUI client that exits in under a second having shown
			// nothing is the signature of a launch that was handed off to
			// another instance somewhere else. Say so loudly.
			if !mapped && lived < time.Second {
				logf("WARNING child %d (%s) exited after %v without a window: "+
					"if this is a GUI app it may have been handed to another instance",
					pid, req.name(), lived.Round(time.Millisecond))
			}
			logf("child %d (%s) exited after %v: %v", pid, req.name(),
				lived.Round(time.Millisecond), err)
			childExit <- pid
		}()
	}
	comp.spawn = func(req spawnReq) { go launch(req) }

	quitOnce := sync.Once{}
	quitCh := make(chan struct{})
	comp.quitFn = func() { quitOnce.Do(func() { close(quitCh) }) }

	// The launcher shares the child pipeline above, so a desktop entry gets
	// exactly the isolation every other child gets: our socket, our runtime
	// dir, our bus. That is not incidental. A launcher runs arbitrary
	// desktop entries whose entire purpose is desktop integration, so it is
	// the single feature most likely to reintroduce the escape that put a
	// file manager on the host desktop.
	if *multi {
		comp.lc = newLauncher(comp, comp.nextImgID)
		comp.nextImgID++
		comp.lc.warm()
	}

	for _, e := range execs {
		launch(spawnReq{argv: []string{"/bin/sh", "-c", e}, label: e})
	}
	if args := flag.Args(); len(args) > 0 {
		launch(spawnReq{argv: args})
	}
	if started == 0 {
		if *multi {
			logf("no initial client; use %s c", *prefix)
		} else {
			fmt.Fprintln(os.Stderr, "wlterm: nothing to run. Try: wlterm -- foot")
			cleanup(headless)
			os.Exit(2)
		}
	}

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)

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

// leakyEnv lists the variables that let a child find its way back to the
// host session: the display, the bus, the runtime dir the bus is autolaunched
// from, and the compositor-specific IPC sockets that let an app drive the
// host window manager directly.
var leakyEnv = map[string]bool{
	"WAYLAND_DISPLAY":             true,
	"WAYLAND_SOCKET":              true,
	"DISPLAY":                     true,
	"XAUTHORITY":                  true,
	"XDG_RUNTIME_DIR":             true,
	"DBUS_SESSION_BUS_ADDRESS":    true,
	"DBUS_STARTER_ADDRESS":        true,
	"DBUS_STARTER_BUS_TYPE":       true,
	"XDG_ACTIVATION_TOKEN":        true,
	"HYPRLAND_INSTANCE_SIGNATURE": true,
	"HYPRLAND_CMD":                true,
	"SWAYSOCK":                    true,
	"I3SOCK":                      true,
	"NIRI_SOCKET":                 true,
	"KDE_FULL_SESSION":            true,
	"KDE_SESSION_UID":             true,
}

// startPrivateBus runs a session bus that only our children can reach.
// Without it, a client whose launcher goes through DBus activation opens its
// window on whatever desktop owns the host bus.
//
// The bus's own environment matters as much as its address. A service the
// bus activates inherits the BUS's environment, not the environment of the
// process that asked for it, so a bus started without WAYLAND_DISPLAY
// activates GTK applications that then die with "cannot open display".
// Launching Thunar through the launcher found exactly that: isolation held
// (nothing reached the host desktop) and the application was unusable.
// So the bus is given our socket too.
func startPrivateBus(dir, waylandSocket string) (addr string, pid int, stop func()) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		logf("dbus-daemon not found; clients get no session bus at all")
		return "", 0, func() {}
	}
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	env := []string{}
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && leakyEnv[k] {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = append(env, "XDG_RUNTIME_DIR="+dir, "WAYLAND_DISPLAY="+waylandSocket)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, func() {}
	}
	if logFile != nil {
		cmd.Stderr = logFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf("private bus failed to start: %v", err)
		return "", 0, func() {}
	}
	line := make([]byte, 4096)
	n, _ := out.Read(line)
	addr = strings.TrimSpace(string(line[:n]))
	if addr == "" {
		cmd.Process.Kill()
		return "", 0, func() {}
	}
	logf("private session bus at %s (pid %d)", addr, cmd.Process.Pid)
	return addr, cmd.Process.Pid, func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
	}
}

// peerIdentity reads the connecting process's credentials off the socket.
// SyscallConn is used rather than conn.File() because File() dups the
// descriptor and puts the connection into blocking mode.
func peerIdentity(conn *net.UnixConn) (pid, sid int) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0, 0
	}
	rc.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err == nil {
			pid = int(cred.Pid)
		}
	})
	if pid > 0 {
		sid = sessionOf(pid)
	}
	return pid, sid
}

// sessionOf reads field 6 of /proc/<pid>/stat, the session id. The comm
// field can contain spaces and parentheses, so parsing starts after its
// closing paren rather than at the first space.
func sessionOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(b[i+1:]))
	// After ')': state ppid pgrp session ...
	if len(f) < 4 {
		return 0
	}
	sid, err := strconv.Atoi(f[3])
	if err != nil {
		return 0
	}
	return sid
}

// launchVerifyAfter is how long an application gets to put a window on
// screen before the launch is reported as unverified.
const launchVerifyAfter = 6 * time.Second

// verifyLaunch checks the framebuffer, not the exit code.
//
// A zero exit means nothing here: the failure mode this exists for is a
// desktop entry that hands its request to an already-running instance
// somewhere else, exits 0, and shows the user nothing. So the check is
// "does a mapped window belong to a process in the session we started",
// which is exactly the question an exit code cannot answer.
func verifyLaunch(comp *compositor, req spawnReq, pid int, startedAt time.Time) {
	time.Sleep(launchVerifyAfter)
	comp.mu.Lock()
	busSid := comp.busSid
	var mine, viaBus, others int
	for _, w := range comp.windows {
		if w.top == nil || w.top.client == nil || w.area.empty() {
			continue
		}
		switch {
		case w.top.client.sid == pid:
			mine++
		case busSid != 0 && w.top.client.sid == busSid:
			// A DBus-activatable application is started by the bus, so its
			// window belongs to the bus's session rather than to the pid we
			// spawned. That is still inside wlterm; it is our bus.
			viaBus++
		default:
			others++
		}
	}
	comp.mu.Unlock()
	if mine > 0 {
		logf("launch VERIFIED: %s (pid %d) has %d window(s) in the framebuffer", req.name(), pid, mine)
		return
	}
	if viaBus > 0 {
		logf("launch VERIFIED via our private bus: %s (pid %d) exited, but %d window(s) "+
			"belong to a service our own bus activated", req.name(), pid, viaBus)
		return
	}
	logf("launch UNVERIFIED: %s (pid %d) put no window on screen in %v "+
		"(%d window(s) on screen belong to other sessions). If this is a GUI "+
		"application it may have been handed to an instance outside wlterm.",
		req.name(), pid, launchVerifyAfter, others)
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

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
