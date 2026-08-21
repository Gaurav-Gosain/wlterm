package main

import (
	"os"
	"path/filepath"
	"testing"
)

// mkTree builds a fake XDG data hierarchy and points the environment at it.
func mkTree(t *testing.T, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	sys1 := filepath.Join(root, "sys1")
	sys2 := filepath.Join(root, "sys2")
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("XDG_DATA_DIRS", sys1+":"+sys2)
	t.Setenv("XDG_CURRENT_DESKTOP", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "C")
}

func byID(res scanResult, id string) *appEntry {
	for i := range res.entries {
		if res.entries[i].id == id {
			return &res.entries[i]
		}
	}
	return nil
}

const minimal = "[Desktop Entry]\nType=Application\nName=%s\nExec=%s\n"

// The first directory to define a desktop file ID wins. This is the whole
// mechanism by which a user overrides a system entry, so it is not optional.
func TestDesktopFileIDFirstWins(t *testing.T) {
	mkTree(t, map[string]string{
		"home/applications/thing.desktop": "[Desktop Entry]\nType=Application\nName=Mine\nExec=mine\n",
		"sys1/applications/thing.desktop": "[Desktop Entry]\nType=Application\nName=Theirs\nExec=theirs\n",
	})
	res := scanApplications()
	e := byID(res, "thing.desktop")
	if e == nil || e.name != "Mine" {
		t.Fatalf("user entry should shadow the system one, got %+v", e)
	}
	if res.shadowed != 1 {
		t.Fatalf("shadowed = %d, want 1", res.shadowed)
	}
}

// A subdirectory contributes its path to the ID with '/' replaced by '-'.
func TestDesktopFileIDFromSubdirectory(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/kde/kate.desktop": "[Desktop Entry]\nType=Application\nName=Kate\nExec=kate\n",
	})
	if e := byID(scanApplications(), "kde-kate.desktop"); e == nil {
		t.Fatal("expected desktop file ID kde-kate.desktop")
	}
}

// Hidden means "treat as deleted"; NoDisplay means "do not show in a menu".
// Both keep the entry out of the launcher, but they are different keys with
// different meanings and are counted separately.
func TestHiddenVersusNoDisplay(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/h.desktop": "[Desktop Entry]\nType=Application\nName=H\nExec=h\nHidden=true\n",
		"sys1/applications/n.desktop": "[Desktop Entry]\nType=Application\nName=N\nExec=n\nNoDisplay=true\n",
		"sys1/applications/k.desktop": "[Desktop Entry]\nType=Application\nName=K\nExec=k\n",
	})
	res := scanApplications()
	if len(res.entries) != 1 || res.entries[0].name != "K" {
		t.Fatalf("entries = %d, want only K", len(res.entries))
	}
	if res.hidden != 1 || res.nodisp != 1 {
		t.Fatalf("hidden=%d nodisplay=%d, want 1 and 1", res.hidden, res.nodisp)
	}
}

// A Hidden user entry must not fall through to the system copy: the
// override is the point.
func TestHiddenOverrideDoesNotFallThrough(t *testing.T) {
	mkTree(t, map[string]string{
		"home/applications/x.desktop": "[Desktop Entry]\nType=Application\nName=X\nExec=x\nHidden=true\n",
		"sys1/applications/x.desktop": "[Desktop Entry]\nType=Application\nName=X\nExec=x\n",
	})
	if len(scanApplications().entries) != 0 {
		t.Fatal("a user Hidden=true entry must suppress the system entry too")
	}
}

// OnlyShowIn and NotShowIn match against a COLON-separated list.
func TestShowInIsAList(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/only.desktop": "[Desktop Entry]\nType=Application\nName=O\nExec=o\nOnlyShowIn=KDE;GNOME;\n",
		"sys1/applications/not.desktop":  "[Desktop Entry]\nType=Application\nName=N\nExec=n\nNotShowIn=GNOME;\n",
	})
	t.Setenv("XDG_CURRENT_DESKTOP", "wlroots:GNOME")
	res := scanApplications()
	if byID(res, "only.desktop") == nil {
		t.Fatal("OnlyShowIn=GNOME should show when GNOME is anywhere in the colon list")
	}
	if byID(res, "not.desktop") != nil {
		t.Fatal("NotShowIn=GNOME should hide when GNOME is anywhere in the colon list")
	}

	t.Setenv("XDG_CURRENT_DESKTOP", "XFCE")
	res = scanApplications()
	if byID(res, "only.desktop") != nil {
		t.Fatal("OnlyShowIn=KDE;GNOME must hide under XFCE")
	}
	if byID(res, "not.desktop") == nil {
		t.Fatal("NotShowIn=GNOME must show under XFCE")
	}
}

// TryExec names a binary that must resolve, otherwise the application is not
// installed and must not be offered.
func TestTryExec(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/ok.desktop":  "[Desktop Entry]\nType=Application\nName=Ok\nExec=x\nTryExec=sh\n",
		"sys1/applications/bad.desktop": "[Desktop Entry]\nType=Application\nName=Bad\nExec=x\nTryExec=definitely-not-a-real-binary-9f3a\n",
	})
	res := scanApplications()
	if byID(res, "ok.desktop") == nil {
		t.Fatal("TryExec=sh should resolve")
	}
	if byID(res, "bad.desktop") != nil {
		t.Fatal("an unresolvable TryExec means the entry is not installed")
	}
	if res.noTry != 1 {
		t.Fatalf("noTry = %d, want 1", res.noTry)
	}
}

func TestLocalisedName(t *testing.T) {
	body := "[Desktop Entry]\nType=Application\nExec=x\n" +
		"Name=Base\nName[de]=Deutsch\nName[de_AT]=Oesterreich\nName[fr]=Francais\n"
	mkTree(t, map[string]string{"sys1/applications/l.desktop": body})

	for _, c := range []struct{ lang, want string }{
		{"C", "Base"},
		{"de_DE.UTF-8", "Deutsch"},
		{"de_AT.UTF-8", "Oesterreich"},
		{"fr_FR.UTF-8", "Francais"},
		{"es_ES.UTF-8", "Base"},
	} {
		t.Setenv("LANG", c.lang)
		e := byID(scanApplications(), "l.desktop")
		if e == nil || e.name != c.want {
			t.Fatalf("LANG=%s gave %v, want %q", c.lang, e, c.want)
		}
	}
}

func TestValueEscapes(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/e.desktop": "[Desktop Entry]\nType=Application\n" +
			"Name=a\\sb\\tc\n Exec = x \nKeywords=one;t\\;wo;three;\n",
	})
	e := byID(scanApplications(), "e.desktop")
	if e == nil {
		t.Fatal("missing entry")
	}
	if e.name != "a b\tc" {
		t.Fatalf("name = %q, want %q", e.name, "a b\tc")
	}
	// "Space before and after the equals sign should be ignored".
	if e.exec != "x " && e.exec != "x" {
		t.Fatalf("exec = %q", e.exec)
	}
	if len(e.keywords) != 3 || e.keywords[1] != "t;wo" {
		t.Fatalf("keywords = %q, want an escaped semicolon inside one element", e.keywords)
	}
}

// Actions become their own rows, and only the names listed in Actions= with
// a matching group are valid.
func TestActions(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/b.desktop": "[Desktop Entry]\nType=Application\nName=Browser\nExec=b\n" +
			"Actions=priv;ghost;\n" +
			"[Desktop Action priv]\nName=New Private Window\nExec=b --private\n",
	})
	res := scanApplications()
	if res.actions != 1 {
		t.Fatalf("actions = %d, want 1 (ghost has no group)", res.actions)
	}
	a := byID(res, "b.desktop:priv")
	if a == nil {
		t.Fatal("missing action row")
	}
	if a.label != "Browser - New Private Window" {
		t.Fatalf("action label = %q", a.label)
	}
	argv, _, err := parseExec(a.exec, a.path, a.name, a.icon)
	if err != nil || len(argv) != 2 || argv[1] != "--private" {
		t.Fatalf("action argv = %q (%v)", argv, err)
	}
}

func TestNonApplicationTypesSkipped(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/link.desktop": "[Desktop Entry]\nType=Link\nName=L\nURL=http://x\n",
		"sys1/applications/dir.desktop":  "[Desktop Entry]\nType=Directory\nName=D\n",
	})
	if n := len(scanApplications().entries); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
}

func TestTerminalTrue(t *testing.T) {
	mkTree(t, map[string]string{
		"sys1/applications/t.desktop": "[Desktop Entry]\nType=Application\nName=T\nExec=htop\nTerminal=true\n",
		"sys1/applications/f.desktop": "[Desktop Entry]\nType=Application\nName=F\nExec=gui\nTerminal=false\n",
		"sys1/applications/j.desktop": "[Desktop Entry]\nType=Application\nName=J\nExec=gui\nTerminal=yes\n",
	})
	res := scanApplications()
	if !byID(res, "t.desktop").term {
		t.Fatal("Terminal=true not honoured")
	}
	if byID(res, "f.desktop").term {
		t.Fatal("Terminal=false read as true")
	}
	// The spec's boolean type is exactly "true" or "false"; anything else is
	// invalid and must not be guessed into a yes.
	if byID(res, "j.desktop").term {
		t.Fatal("Terminal=yes is not a valid boolean and must not mean true")
	}
}

func TestFoldASCII(t *testing.T) {
	if got := foldASCII("Bücher"); got != "Bucher" {
		t.Fatalf("foldASCII = %q", got)
	}
	if got := foldASCII("plain"); got != "plain" {
		t.Fatalf("foldASCII = %q", got)
	}
}
