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
	autoWhy := "explicit"

	layers := flag.String("layers", "per-window", "granularity: per-window|single")
	fps := flag.Int("fps", 120, "max frames per second, 0 for uncapped")
	logPath := flag.String("log", "", "debug log file")
	pixels := flag.String("pixels", "", "WxH: headless mode, no tty setup")
	cellSize := flag.String("cell", "10x20", "headless cell size WxH")
	socketName := flag.String("socket", "", "wayland socket name (default wlterm-PID)")
	stampPath := flag.String("stamp", "", "write per-frame wallclock ms to this file")
	multi := flag.Bool("multi", false, "multi-surface tiling mode: chrome, leader key, launcher")
	fullscreenState := flag.Bool("fullscreen", false, "tell the app it is fullscreen; browsers then hide their own toolbars")
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
	prSetChildSubreaper()
	sweepOrphanShm()

	comp := &compositor{
		single:      !*multi,
		fullscreen:  *fullscreenState,
		clients:     map[*client]bool{},
		windowsGone: make(chan struct{}, 1),
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
		*mode, autoWhy = pickTransport(p.frameEdits)
	}
	// Only the chosen-for-you path gets the ceiling. An explicit -mode delta
	// is somebody who knows what the transport is, and -fps stays the measured
	// promise it is documented to be for them.
	if *mode == "delta" && autoWhy != "explicit" && (*fps == 0 || *fps > deltaMaxFPS) {
		// Frame edits are cheap enough to remove wlterm's own brake, and
		// nothing downstream supplies another one: wlterm never waits for the
		// host, so a transport that costs nothing lets it run until the host
		// has no CPU left.
		//
		// Measured on this machine, wlterm uncapped in delta mode against a
		// software-rendered kitty: 150 and 200 frames a second both kept the
		// picture moving, 260 dropped more than half of it, and uncapped -- 261
		// achieved -- left the terminal presenting nothing at all for twenty
		// seconds. The same workload in shm mode never got there, because
		// copying the frame is its own brake and it tops out near 130.
		//
		// So delta keeps a ceiling even when asked not to. A picture the host
		// cannot draw is not worth producing, and the whole point of the
		// transport is that it does not need a high rate to look smooth.
		logf("delta: capping at %d fps (asked for %s); frame edits are cheap "+
			"enough to starve the terminal of the time it needs to draw them",
			deltaMaxFPS, orUncapped(*fps))
		*fps = deltaMaxFPS
	}
	kind := "single-app"
	if *multi {
		kind = "multi-surface"
	}
	quitDesc := "none (the app exiting, or a signal, is the only way out)"
	if p.quitKey != 0 {
		quitDesc = p.quitName + " twice within " + quitWindow.String()
	}
	logf("%s: mode=%s (%s) layers=%s size=%dx%d cell=%dx%d leader=%s quit=%s",
		kind, *mode, autoWhy, *layers, comp.widthPx, comp.heightPx, comp.cellW, comp.cellH,
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
		// The name is short on purpose. A unix socket path is 107 bytes,
		// and a nested compositor builds its own sockets under this
		// directory: Hyprland's event socket needs 61 bytes of instance
		// signature on top of it and stops working if the whole path does
		// not fit.
		childRuntime = fmt.Sprintf("%s/wl-%d", runtimeDir, os.Getpid())
		if err := os.MkdirAll(childRuntime, 0o700); err != nil {
			fatal("private runtime dir: %v", err)
		}
		defer func() {
			unmountUnder(childRuntime)
			os.RemoveAll(childRuntime)
		}()
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
	have := map[string]bool{}
	for _, e := range os.Environ() {
		k, _, ok := strings.Cut(e, "=")
		if ok && leakyEnv[k] {
			continue
		}
		if ok {
			have[k] = true
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
	// Point each toolkit at Wayland. There is no X server here and no
	// XWayland, so a toolkit that picks X11 by default does not start at
	// all. Setting these means the user does not have to remember a flag
	// for every application. A variable the user set is kept: this fills
	// gaps, it does not overrule a choice.
	for _, kv := range toolkitEnv {
		k, _, _ := strings.Cut(kv, "=")
		if !have[k] {
			childEnv = append(childEnv, kv)
		}
	}
	logf("isolation: runtime_dir=%s bus=%q", childRuntime, busAddr)

	var childMu sync.Mutex
	children := map[int]*exec.Cmd{}
	// sessions keeps every session we ever started, including the ones
	// whose leader has already exited. A launcher binary forks the real
	// application and returns, so the leader is gone long before the window
	// is, and the process to signal on the way out is still in that
	// session.
	sessions := map[int]bool{}
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
		sessions[pid] = true
		started++
		childMu.Unlock()
		logf("spawned %q pid=%d argv=%q", req.name(), pid, argv)
		startedAt := time.Now()
		comp.mu.Lock()
		windowsBefore := len(comp.windows)
		comp.mu.Unlock()
		// Setsid above makes the child a session leader, so its session id
		// equals its pid and every descendant inherits it. That is the
		// handle used to verify the launch below.
		go verifyLaunch(comp, req, pid, windowsBefore)
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

	// orphaned: every process we started has exited, so the last window
	// closing is what ends the session. grace is the wait for a window that
	// has not appeared yet: a launcher can return before the application it
	// started has drawn anything.
	orphaned := false
	var grace <-chan time.Time

	// killAll takes the whole tree with us. The process group is not
	// enough on its own: Electron and Chromium put the process that owns
	// the window into a session of their own, so the group we started is
	// empty by the time it matters. VS Code outlived wlterm that way, still
	// drawing into a socket nobody was listening on.
	killAll := func() {
		childMu.Lock()
		mine := map[int]bool{}
		for pid := range sessions {
			mine[pid] = true
			syscall.Kill(-pid, syscall.SIGTERM)
		}
		childMu.Unlock()
		killTree(mine)
	}

	for {
		select {
		case pid := <-childExit:
			childMu.Lock()
			delete(children, pid)
			left := len(children)
			childMu.Unlock()
			if left != 0 || started == 0 {
				continue
			}
			// The process we started is gone. That is not the same as the
			// application being gone: `code`, `thunar` and most desktop
			// launchers start the real program and return straight away.
			// So the question is whether anything is still on screen.
			comp.mu.Lock()
			onScreen := len(comp.windows)
			everMapped := comp.windowsAdded > 0
			comp.mu.Unlock()
			orphaned = true
			if onScreen > 0 {
				logf("every process we started has exited, but %d window(s) "+
					"are still up: staying until the last one closes",
					onScreen)
				continue
			}
			if everMapped {
				// The application had a window and closed it. That is the
				// ordinary way out and there is nothing to wait for.
				time.Sleep(50 * time.Millisecond) // let the last frame drain
				killAll()
				cleanup(headless)
				return
			}
			// Nothing has ever been on screen. A launcher that has already
			// returned may still be starting the application, so wait
			// before deciding that nothing is coming.
			logf("every process we started has exited with nothing on screen: "+
				"waiting %v for a window", orphanGrace)
			grace = time.After(orphanGrace)
		case <-grace:
			grace = nil
			comp.mu.Lock()
			onScreen := len(comp.windows)
			comp.mu.Unlock()
			if onScreen > 0 {
				logf("%d window(s) appeared: staying until the last one closes", onScreen)
				continue
			}
			logf("no window appeared in %v", orphanGrace)
			time.Sleep(50 * time.Millisecond)
			killAll()
			cleanup(headless)
			return
		case <-comp.windowsGone:
			if !orphaned {
				continue
			}
			logf("the last window closed")
			time.Sleep(50 * time.Millisecond)
			killAll()
			cleanup(headless)
			return
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
				// Re-probe rather than trusting TIOCGWINSZ alone: inside a
				// multiplexer the pane's pixel size comes from the host's
				// XTWINOPS answer, and tuios answers per pane.
				os.Stdout.WriteString("\x1b[16t\x1b[14t")
				go func() {
					time.Sleep(150 * time.Millisecond)
					comp.mu.Lock()
					was := fmt.Sprintf("%dx%d", comp.widthPx, comp.heightPx)
					computeSize(comp, p)
					now := fmt.Sprintf("%dx%d", comp.widthPx, comp.heightPx)
					if was != now {
						logf("SIGWINCH: pane %s -> %s px, reconfiguring the toplevel", was, now)
					} else {
						logf("SIGWINCH: pane still %s px", now)
					}
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
	// Not a socket, but it names the host desktop, and a portal reads it to
	// decide which backend to load. Inherited, the portal loads the host
	// compositor's backend, which then goes looking for the host's IPC
	// socket. toolkitEnv puts wlterm's own name back.
	"XDG_CURRENT_DESKTOP": true,
	"XDG_SESSION_DESKTOP": true,
	"DESKTOP_SESSION":     true,
}

// toolkitEnv tells each toolkit to use Wayland, and tells the desktop
// portal which desktop it is on. Only set where the parent environment does
// not already have the variable.
//
// XDG_CURRENT_DESKTOP matters as much as the backend variables. It is what a
// portal reads to choose an implementation, and inheriting the host's value
// makes the portal load the host desktop's backend, which then looks for the
// host compositor's IPC socket. wlterm is its own desktop, so it says so.
var toolkitEnv = []string{
	"XDG_SESSION_TYPE=wayland",
	"XDG_CURRENT_DESKTOP=wlterm",
	"GDK_BACKEND=wayland",
	"QT_QPA_PLATFORM=wayland",
	"SDL_VIDEODRIVER=wayland",
	"CLUTTER_BACKEND=wayland",
	"MOZ_ENABLE_WAYLAND=1",
	"ELECTRON_OZONE_PLATFORM_HINT=wayland",
	"_JAVA_AWT_WM_NONREPARENTING=1",
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
	env = append(env, "XDG_RUNTIME_DIR="+dir, "WAYLAND_DISPLAY="+waylandSocket)
	// A service the bus activates inherits this, so it needs the same
	// toolkit settings the direct children get.
	for _, kv := range toolkitEnv {
		k, _, _ := strings.Cut(kv, "=")
		if os.Getenv(k) == "" || leakyEnv[k] {
			env = append(env, kv)
		}
	}
	cmd.Env = env
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
const launchVerifyAfter = 15 * time.Second

// orphanGrace is how long wlterm waits for a window after every process it
// started has exited. A desktop launcher returns in about a second and the
// application it started draws a second or two later, so exiting the moment
// the process does would kill it before it appeared.
const orphanGrace = 10 * time.Second

// verifyLaunch checks the framebuffer, not the exit code.
//
// A zero exit means nothing here: the failure mode this exists for is a
// desktop entry that hands its request to an already-running instance
// somewhere else, exits 0, and shows the user nothing. So the check is
// "does a mapped window belong to a process in the session we started",
// which is exactly the question an exit code cannot answer.
func verifyLaunch(comp *compositor, req spawnReq, pid, windowsBefore int) {
	// Poll rather than sample once. A browser on a busy machine can take
	// ten seconds to draw, and a single look at six seconds reported a
	// launch as failed while it was still starting.
	var mine, viaBus, others, total int
	deadline := time.Now().Add(launchVerifyAfter)
	for {
		mine, viaBus, others = 0, 0, 0
		comp.mu.Lock()
		busSid := comp.busSid
		total = len(comp.windows)
		for _, w := range comp.windows {
			if w.top == nil || w.top.client == nil || w.area.empty() {
				continue
			}
			switch {
			case w.top.client.sid == pid:
				mine++
			case busSid != 0 && w.top.client.sid == busSid:
				// A DBus-activatable application is started by the bus, so
				// its window belongs to the bus's session rather than to
				// the pid we spawned. That is still inside wlterm; it is
				// our bus.
				viaBus++
			default:
				others++
			}
		}
		comp.mu.Unlock()
		if mine > 0 || viaBus > 0 || total > windowsBefore {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if mine > 0 {
		logf("launch VERIFIED: %s (pid %d) has %d window(s) in the framebuffer", req.name(), pid, mine)
		return
	}
	if viaBus > 0 {
		logf("launch VERIFIED via our private bus: %s (pid %d) exited, but %d window(s) "+
			"belong to a service our own bus activated", req.name(), pid, viaBus)
		return
	}
	// A browser and an Electron application both start their own session
	// for the process that owns the window, so the session we started is
	// not the one on screen. The window count is the check that still
	// works: something appeared here that was not here before.
	if total > windowsBefore {
		logf("launch VERIFIED by count: %s (pid %d) is in a session of its own, "+
			"and the pane went from %d window(s) to %d",
			req.name(), pid, windowsBefore, total)
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

// deltaMaxFPS is the ceiling the delta transport keeps when -mode auto chose
// it. See where it is applied for the measurements behind the number.
const deltaMaxFPS = 200

// orUncapped names a frame cap for a log line.
func orUncapped(fps int) string {
	if fps == 0 {
		return "uncapped"
	}
	return strconv.Itoa(fps)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// muxEnv names the variables a terminal multiplexer exports into every pane.
// Their presence is what tells us the terminal answering our probes is not
// the terminal that will finally draw the pixels.
//
// tuios is not in this list. It is the one multiplexer that does carry frame
// edits, and it says so itself; see pickTransport.
var muxEnv = []string{"TMUX", "ZELLIJ", "STY"}

// tuiosEnv names the variables tuios exports into every pane. There are two
// because there are two paths: TUIOS_WINDOW_ID from the in-process one and
// TUIOS_SESSION from the daemon. Nothing else about a tuios pane gives it
// away -- TERM is inherited from the host, and TERM_PROGRAM is deliberately
// set to "ghostty" so that guests will use kitty graphics.
var tuiosEnv = []string{"TUIOS_WINDOW_ID", "TUIOS_SESSION"}

// pickTransport chooses a kitty transport, and returns why.
//
// Delta mode patches an image in place with kitty animation frames (a=f),
// which is the difference between 81 bytes and a couple of megabytes per small
// update. The question is only ever whether the thing at the far end of the
// pty applies them, and there are three ways to find out, in order of how much
// they can be trusted:
//
//   - Ask the terminal. frameEdits is the answer to a probe that a terminal
//     which gets frame edits wrong has to fail, so a plain OK is not enough to
//     pass it. This is the answer used whenever there is no multiplexer.
//   - Ask the multiplexer. tuios forwards a guest's a=f to its own host and
//     exports the result as TUIOS_KITTY_ANIMATION, so the pane can be asked
//     what the pane carries. It has to be asked rather than probed, because
//     tuios does not relay the host's reply back into the pane: a guest that
//     sends a frame edit and waits hears nothing whether it worked or not.
//   - Guess from the environment. This is what used to happen and it is
//     wrong. KITTY_WINDOW_ID is inherited straight through a pane, and tuios
//     forwards the host's TERM into it rather than replacing it, so both name
//     the host terminal and neither says anything about the pane in front of
//     it.
//
// Everything else -- tmux, zellij, screen, an unknown terminal, headless --
// gets shared memory, which is correct everywhere.
func pickTransport(frameEdits bool) (mode, why string) {
	for _, k := range tuiosEnv {
		if os.Getenv(k) == "" {
			continue
		}
		switch os.Getenv("TUIOS_KITTY_ANIMATION") {
		case "1":
			return "delta", "tuios says this pane carries frame edits"
		case "0":
			return "shm", "tuios says this pane does not carry frame edits"
		}
		return "shm", "inside tuios, which did not say whether frame edits get through"
	}
	for _, k := range muxEnv {
		if os.Getenv(k) != "" {
			return "shm", "inside " + k + ", frame edits are not passed through"
		}
	}
	if frameEdits {
		return "delta", "the terminal applied a test frame edit and refused a bad one"
	}
	return "shm", "shared memory is safe everywhere"
}

// ---- taking the whole tree with us ----

// prSetChildSubreaper makes orphaned descendants reparent to wlterm instead
// of to init. Without it a program that double-forks -- Electron and every
// Chromium do -- is no longer reachable from here the moment its launcher
// exits, and it outlives the compositor it was drawing into. VS Code did
// exactly that: wlterm went away and the window's process kept running
// against a socket nobody was listening on.
func prSetChildSubreaper() {
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		logf("child subreaper unavailable: %v", errno)
	}
}

// procParents reads every process's parent from /proc.
func procParents() map[int]int {
	parents := map[int]int{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return parents
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// The comm field can hold spaces and parentheses, so parsing starts
		// after its closing paren: state, then ppid.
		i := strings.LastIndexByte(string(b), ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		if len(f) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err == nil {
			parents[pid] = ppid
		}
	}
	return parents
}

// descendants lists every process below us, plus anything left in a session
// we started. Two answers to the same question, because a process that
// changed its session is still our child, and a child that outran the
// subreaper is still in our session.
func descendants(sessions map[int]bool) []int {
	me := os.Getpid()
	parents := procParents()
	children := map[int][]int{}
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	seen := map[int]bool{}
	var walk func(int)
	walk = func(pid int) {
		for _, c := range children[pid] {
			if c == me || seen[c] {
				continue
			}
			seen[c] = true
			walk(c)
		}
	}
	walk(me)
	for sid := range sessions {
		for pid := range parents {
			if pid == me || seen[pid] {
				continue
			}
			if sessionOf(pid) == sid {
				seen[pid] = true
				walk(pid)
			}
		}
	}
	out := make([]int, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	return out
}

// killTree asks everything we started to stop, then insists. A Wayland
// client usually exits on its own when the socket closes, and some do not.
func killTree(sessions map[int]bool) {
	pids := descendants(sessions)
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	left := 0
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil {
			syscall.Kill(pid, syscall.SIGKILL)
			left++
		}
	}
	if left > 0 {
		logf("killed %d process(es) that did not stop when asked", left)
	}
}

// unmountUnder drops any fuse mount a child left inside our private runtime
// directory. xdg-document-portal and gvfs both mount there, and a mount is
// why the directory outlived the process that made it.
func unmountUnder(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := dir + "/" + e.Name()
		if err := syscall.Rmdir(p); err == nil || err == syscall.ENOTEMPTY {
			continue // not a mount point
		}
		exec.Command("fusermount3", "-u", p).Run()
	}
}
