package main

// Desktop Entry Specification, the parts a launcher must get right.
//
// This is not a config file format you can skim, so this file follows the
// spec section by section and says where it stops. References are to the
// freedesktop.org Desktop Entry Specification 1.5.
//
//   * Basic format: groups in [brackets], key=value lines, # comments,
//     "space before and after the equals sign should be ignored".
//   * Value types: `string` and `localestring` carry their own escapes
//     (\s \n \t \r \\), which are unescaped BEFORE any further parsing.
//     Exec is a `string`, so its value is unescaped once here and then
//     tokenized again by the Exec rules in execparse.go. Two layers, in
//     that order; conflating them is how a launcher ends up mangling
//     backslashes.
//   * Localised keys: Name[lang_COUNTRY@MODIFIER] and its three shorter
//     forms, matched against LC_ALL / LC_MESSAGES / LANG in the spec's
//     precedence order.
//   * Hidden=true means "the file is deleted"; NoDisplay=true means "do not
//     put me in a menu". They are different keys with different meanings
//     and both are honoured, separately.
//   * OnlyShowIn / NotShowIn are matched against $XDG_CURRENT_DESKTOP,
//     which is a COLON-separated list, not a single name.
//   * TryExec names a binary that must resolve in PATH; if it does not, the
//     entry is not installed and must not be offered.
//   * Desktop file IDs: the path relative to an applications directory with
//     '/' replaced by '-'. The FIRST directory in the search order to
//     define an ID wins, which is the whole mechanism by which a user
//     overrides a system entry.
//   * Actions: each name in Actions= must have a matching
//     [Desktop Action <name>] group. We surface them as their own rows.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ---- raw file model ----

type entryGroup struct {
	name string
	keys map[string]string // key (with any [locale] suffix intact) -> raw value
	ord  []string
}

type entryFile struct {
	path   string
	id     string // desktop file ID
	groups map[string]*entryGroup
	order  []string
}

func (f *entryFile) group(name string) *entryGroup { return f.groups[name] }

// raw returns a key's value with the string escapes already resolved.
func (g *entryGroup) raw(key string) (string, bool) {
	if g == nil {
		return "", false
	}
	v, ok := g.keys[key]
	if !ok {
		return "", false
	}
	return unescapeValue(v), true
}

func (g *entryGroup) str(key string) string { v, _ := g.raw(key); return v }

func (g *entryGroup) boolean(key string) bool {
	// The spec's `boolean` type is exactly "true" or "false". Anything else
	// is invalid; treat invalid as false rather than guessing.
	v, _ := g.raw(key)
	return v == "true"
}

// list splits a `string(s)` value on unescaped semicolons. The spec escapes
// a literal semicolon as "\;", and that escape survives unescapeValue
// (which only handles \s \n \t \r \\), so we split before unescaping the
// remaining "\;".
func (g *entryGroup) list(key string) []string {
	v, ok := g.keys[key]
	if !ok {
		return nil
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) && v[i+1] == ';' {
			cur.WriteByte(';')
			i++
			continue
		}
		if v[i] == ';' {
			out = append(out, unescapeValue(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(v[i])
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, unescapeValue(s))
	}
	return out
}

// localizedList is list() with the spec's locale precedence applied first,
// for the `localestring(s)` keys (Keywords is one).
func (g *entryGroup) localizedList(key string, loc locale) []string {
	if g == nil {
		return nil
	}
	for _, suffix := range loc.candidates() {
		if _, ok := g.keys[key+"["+suffix+"]"]; ok {
			return g.list(key + "[" + suffix + "]")
		}
	}
	return g.list(key)
}

// localized resolves key[locale] per the spec's matching order:
// lang_COUNTRY@MODIFIER, lang_COUNTRY, lang@MODIFIER, lang, then unlocalized.
func (g *entryGroup) localized(key string, loc locale) string {
	if g == nil {
		return ""
	}
	for _, suffix := range loc.candidates() {
		if v, ok := g.raw(key + "[" + suffix + "]"); ok {
			return v
		}
	}
	return g.str(key)
}

// unescapeValue resolves the escapes the spec defines for `string` and
// `localestring` values. Note that "\;" is deliberately left alone here: it
// is a list separator escape and is handled by list().
func unescapeValue(v string) string {
	if !strings.ContainsRune(v, '\\') {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 >= len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case 's':
			b.WriteByte(' ')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		default:
			// Not a defined escape: keep both bytes, so "\;" survives for
			// list() and an Exec value's own backslashes reach execparse.
			b.WriteByte('\\')
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// parseEntryFile reads one .desktop file into groups. Duplicate keys inside
// a group keep the first, matching the "should not" of the spec without
// erroring out on files that do it anyway.
func parseEntryFile(path string) (*entryFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ef := &entryFile{path: path, groups: map[string]*entryGroup{}}
	var cur *entryGroup
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		// A UTF-8 BOM at the head of the file is not part of the group name.
		line = strings.TrimPrefix(line, "\ufeff")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			name := trimmed[1 : len(trimmed)-1]
			if g, ok := ef.groups[name]; ok {
				cur = g
			} else {
				cur = &entryGroup{name: name, keys: map[string]string{}}
				ef.groups[name] = cur
				ef.order = append(ef.order, name)
			}
			continue
		}
		if cur == nil {
			continue // keys before any group header are not valid
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimLeft(line[eq+1:], " \t")
		if key == "" {
			continue
		}
		if _, dup := cur.keys[key]; dup {
			continue
		}
		cur.keys[key] = val
		cur.ord = append(cur.ord, key)
	}
	return ef, sc.Err()
}

// ---- locale ----

type locale struct{ lang, country, modifier string }

func currentLocale() locale {
	v := ""
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if s := os.Getenv(k); s != "" {
			v = s
			break
		}
	}
	if v == "" || v == "C" || v == "POSIX" {
		return locale{}
	}
	var l locale
	if i := strings.IndexByte(v, '@'); i >= 0 {
		l.modifier = v[i+1:]
		v = v[:i]
	}
	// The encoding (".UTF-8") is stripped and never used for matching.
	if i := strings.IndexByte(v, '.'); i >= 0 {
		v = v[:i]
	}
	if i := strings.IndexByte(v, '_'); i >= 0 {
		l.country = v[i+1:]
		v = v[:i]
	}
	l.lang = v
	return l
}

func (l locale) candidates() []string {
	if l.lang == "" {
		return nil
	}
	var out []string
	if l.country != "" && l.modifier != "" {
		out = append(out, l.lang+"_"+l.country+"@"+l.modifier)
	}
	if l.country != "" {
		out = append(out, l.lang+"_"+l.country)
	}
	if l.modifier != "" {
		out = append(out, l.lang+"@"+l.modifier)
	}
	return append(out, l.lang)
}

// ---- the launcher's view of an entry ----

// appEntry is one row in the launcher: either a whole application or one of
// its Actions. Actions carry the parent's identity but their own Exec.
type appEntry struct {
	id       string // desktop file ID, plus ":action" for actions
	fileID   string // the owning desktop file ID
	path     string
	name     string // localised Name (or the action's Name)
	generic  string // GenericName
	comment  string // Comment
	action   string // action identifier, empty for the main entry
	exec     string // raw Exec value, spec-unescaped but not yet tokenized
	tryExec  string
	workDir  string // Path=
	icon     string
	term     bool // Terminal=true
	keywords []string

	// Fields the launcher actually draws and matches against, folded to the
	// ASCII the chrome font atlas can render.
	label string
	sub   string
	hay   []string // secondary match fields (exec basename, generic, keywords)
}

// ---- scanning ----

// applicationDirs returns the applications directories in spec precedence:
// $XDG_DATA_HOME first, then each entry of $XDG_DATA_DIRS in order.
func applicationDirs() []string {
	home := os.Getenv("XDG_DATA_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".local", "share")
		}
	}
	dirs := os.Getenv("XDG_DATA_DIRS")
	if dirs == "" {
		dirs = "/usr/local/share:/usr/share"
	}
	var out []string
	if home != "" {
		out = append(out, filepath.Join(home, "applications"))
	}
	for _, d := range strings.Split(dirs, ":") {
		if d == "" {
			continue
		}
		out = append(out, filepath.Join(d, "applications"))
	}
	return out
}

// currentDesktops is $XDG_CURRENT_DESKTOP as the colon-separated list it
// actually is. wlterm deliberately does NOT override this for its own
// filtering: the point of the launcher is to offer the same applications
// the user's normal launcher offers, and rewriting it to "wlterm" would
// hide every entry that carries OnlyShowIn=GNOME and friends.
func currentDesktops() []string {
	v := os.Getenv("XDG_CURRENT_DESKTOP")
	if v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ":") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type scanResult struct {
	entries  []appEntry
	files    int // .desktop files read
	hidden   int // Hidden=true
	nodisp   int // NoDisplay=true
	shadowed int // lost the desktop-file-ID race to an earlier directory
	notShown int // OnlyShowIn/NotShowIn excluded
	noTry    int // TryExec did not resolve
	badType  int // not Type=Application
	actions  int // action rows produced
}

// scanApplications walks the applications directories and builds the
// launcher's list.
func scanApplications() scanResult {
	loc := currentLocale()
	desktops := currentDesktops()
	seen := map[string]bool{} // desktop file ID -> already defined
	var res scanResult

	for _, dir := range applicationDirs() {
		root, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(p, ".desktop") {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			// Desktop file ID: relative path, separators become dashes.
			id := strings.ReplaceAll(filepath.ToSlash(rel), "/", "-")
			if seen[id] {
				res.shadowed++
				return nil
			}
			// Mark the ID as taken even if the file turns out unusable: a
			// user's broken override is still an override, and falling
			// through to the system copy would defeat the mechanism.
			seen[id] = true
			res.files++

			ef, perr := parseEntryFile(p)
			if perr != nil {
				return nil
			}
			ef.id = id
			g := ef.group("Desktop Entry")
			if g == nil {
				return nil
			}
			if t := g.str("Type"); t != "Application" {
				res.badType++
				return nil
			}
			// Hidden means the file is to be treated as if it did not
			// exist at all. It is not the same as NoDisplay.
			if g.boolean("Hidden") {
				res.hidden++
				return nil
			}
			if !showIn(g, desktops) {
				res.notShown++
				return nil
			}
			try := g.str("TryExec")
			if try != "" && !resolvesInPath(try) {
				res.noTry++
				return nil
			}
			// NoDisplay: installed and launchable, but not offered in a
			// menu. The launcher is a menu, so it is filtered here and
			// counted, rather than being conflated with Hidden.
			if g.boolean("NoDisplay") {
				res.nodisp++
				return nil
			}
			execVal := g.str("Exec")
			if strings.TrimSpace(execVal) == "" {
				return nil
			}

			name := g.localized("Name", loc)
			if name == "" {
				name = strings.TrimSuffix(rel, ".desktop")
			}
			base := appEntry{
				id: id, fileID: id, path: p,
				name:     name,
				generic:  g.localized("GenericName", loc),
				comment:  g.localized("Comment", loc),
				exec:     execVal,
				tryExec:  try,
				workDir:  g.str("Path"),
				icon:     g.str("Icon"),
				term:     g.boolean("Terminal"),
				keywords: g.localizedList("Keywords", loc),
			}
			res.entries = append(res.entries, finishEntry(base))

			// Actions. Only names listed in Actions= are valid, and each
			// needs its own [Desktop Action <name>] group.
			for _, an := range g.list("Actions") {
				ag := ef.group("Desktop Action " + an)
				if ag == nil {
					continue
				}
				if ag.boolean("Hidden") || ag.boolean("NoDisplay") {
					continue
				}
				aexec := ag.str("Exec")
				if strings.TrimSpace(aexec) == "" {
					continue
				}
				a := base
				a.id = id + ":" + an
				a.action = an
				a.exec = aexec
				a.icon = ag.str("Icon")
				a.name = name
				a.comment = ""
				// The action's own Name is the verb; without one the
				// action identifier itself is all we have to show.
				a.generic = ag.localized("Name", loc)
				if a.generic == "" {
					a.generic = an
				}
				res.actions++
				res.entries = append(res.entries, finishEntry(a))
			}
			return nil
		})
	}
	sort.Slice(res.entries, func(i, j int) bool {
		a, b := res.entries[i], res.entries[j]
		if a.label != b.label {
			return strings.ToLower(a.label) < strings.ToLower(b.label)
		}
		return a.id < b.id
	})
	return res
}

// finishEntry computes the display label, the sub-line, and the secondary
// match fields. Everything the chrome layer draws is folded to ASCII here,
// once, so a match position is a glyph position with no further mapping.
func finishEntry(e appEntry) appEntry {
	e.label = foldASCII(e.name)
	if e.action != "" {
		// tuios-flavoured: the action reads as a modifier on its parent,
		// the way rofi lists "Firefox (New Private Window)".
		verb := e.generic
		if verb == "" {
			verb = e.action
		}
		e.label = foldASCII(e.name) + " - " + foldASCII(verb)
	}
	sub := e.generic
	if e.action != "" {
		sub = ""
	}
	if sub == "" {
		sub = e.comment
	}
	if sub == "" {
		if argv, _, err := parseExec(e.exec, e.path, e.name, e.icon); err == nil && len(argv) > 0 {
			sub = argv[0]
		}
	}
	e.sub = foldASCII(sub)

	// Secondary haystack: things a person plausibly types that are not on
	// screen. Matches here score lower and are not highlighted, because the
	// positions would not correspond to the drawn label.
	hay := map[string]bool{}
	if argv, _, err := parseExec(e.exec, e.path, e.name, e.icon); err == nil && len(argv) > 0 {
		hay[foldASCII(filepath.Base(argv[0]))] = true
	}
	if e.generic != "" {
		hay[foldASCII(e.generic)] = true
	}
	for _, k := range e.keywords {
		hay[foldASCII(k)] = true
	}
	if strings.HasSuffix(e.fileID, ".desktop") {
		hay[foldASCII(strings.TrimSuffix(e.fileID, ".desktop"))] = true
	}
	for k := range hay {
		if k != "" && !strings.EqualFold(k, e.label) {
			e.hay = append(e.hay, k)
		}
	}
	sort.Strings(e.hay)
	return e
}

// showIn implements OnlyShowIn / NotShowIn against the colon-separated
// $XDG_CURRENT_DESKTOP list. Per the spec, comparison is by exact string.
func showIn(g *entryGroup, desktops []string) bool {
	if only := g.list("OnlyShowIn"); len(only) > 0 {
		for _, d := range desktops {
			for _, o := range only {
				if d == o {
					return true
				}
			}
		}
		return false
	}
	for _, n := range g.list("NotShowIn") {
		for _, d := range desktops {
			if d == n {
				return false
			}
		}
	}
	return true
}

// resolvesInPath implements TryExec: "if the name is not an absolute path,
// the $PATH environment variable is consulted".
func resolvesInPath(name string) bool {
	if strings.ContainsRune(name, '/') {
		st, err := os.Stat(name)
		return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// foldASCII maps a string into the ASCII range the bitmap atlas can draw.
// Latin-1 letters fold to their base letter so "Bücher" is still findable by
// typing "buc"; anything else becomes '?', which is what truncate() would
// have done anyway.
func foldASCII(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] < 32 || s[i] > 126 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 32 && r <= 126:
			b.WriteRune(r)
		case r < 32:
			b.WriteByte(' ')
		default:
			if f, ok := latinFold[r]; ok {
				b.WriteString(f)
			} else {
				b.WriteByte('?')
			}
		}
	}
	return strings.TrimSpace(b.String())
}

var latinFold = map[rune]string{
	'À': "A", 'Á': "A", 'Â': "A", 'Ã': "A", 'Ä': "A", 'Å': "A", 'Æ': "AE",
	'Ç': "C", 'È': "E", 'É': "E", 'Ê': "E", 'Ë': "E", 'Ì': "I", 'Í': "I",
	'Î': "I", 'Ï': "I", 'Ð': "D", 'Ñ': "N", 'Ò': "O", 'Ó': "O", 'Ô': "O",
	'Õ': "O", 'Ö': "O", 'Ø': "O", 'Ù': "U", 'Ú': "U", 'Û': "U", 'Ü': "U",
	'Ý': "Y", 'Þ': "Th", 'ß': "ss",
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'æ': "ae",
	'ç': "c", 'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ì': "i", 'í': "i",
	'î': "i", 'ï': "i", 'ð': "d", 'ñ': "n", 'ò': "o", 'ó': "o", 'ô': "o",
	'õ': "o", 'ö': "o", 'ø': "o", 'ù': "u", 'ú': "u", 'û': "u", 'ü': "u",
	'ý': "y", 'þ': "th", 'ÿ': "y",
	'Ā': "A", 'ā': "a", 'Ē': "E", 'ē': "e", 'Ī': "I", 'ī': "i",
	'Ō': "O", 'ō': "o", 'Ū': "U", 'ū': "u", 'Ś': "S", 'ś': "s",
	'Š': "S", 'š': "s", 'Ž': "Z", 'ž': "z", 'Č': "C", 'č': "c",
	'Ł': "L", 'ł': "l", 'Ń': "N", 'ń': "n", 'Ř': "R", 'ř': "r",
	'–': "-", '—': "-", '‘': "'", '’': "'",
	'“': "\"", '”': "\"", '…': "...", '\u00a0': " ",
}

func (r scanResult) summary() string {
	return fmt.Sprintf("apps=%d (files=%d actions=%d) filtered: hidden=%d nodisplay=%d "+
		"showin=%d tryexec=%d nonapp=%d shadowed=%d",
		len(r.entries), r.files, r.actions, r.hidden, r.nodisp,
		r.notShown, r.noTry, r.badType, r.shadowed)
}
