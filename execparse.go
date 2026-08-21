package main

// The Exec key.
//
// Exec= is NOT a shell command line. It has its own quoting rules and its
// own field codes, and handing it to `sh -c` is both wrong and a command
// injection vector, because .desktop files can be dropped anywhere on the
// XDG search path by anything. wlterm tokenizes per the spec and execs the
// resulting argv directly, with no shell anywhere in the path.
//
// Two consequences worth stating, because they look like bugs and are not:
//
//   * `Exec=notice-cmd && /usr/lib/real-app %u` (a real entry on this
//     machine) runs `notice-cmd` with the literal arguments "&&",
//     "/usr/lib/real-app". `&` is a reserved character that the spec
//     requires to be quoted; an unquoted one has no shell meaning because
//     there is no shell. GIO behaves the same way. We log it.
//   * A single quote is not a quoting character in this format. Only the
//     double quote is. `'` must itself be escaped or double-quoted.
//
// Quoting rules (spec, "The Exec key"):
//   Arguments are separated by whitespace. An argument may be enclosed in
//   double quotes; inside double quotes, a backslash escapes the characters
//   `"`, `` ` ``, `$` and `\`. The characters
//   `" ' \ > < ~ | & ; $ * ? # ( )` and backtick are reserved.
//
// Field codes:
//   %f %F   a file / a list of files      -> removed (we launch with none)
//   %u %U   a URL / a list of URLs        -> removed
//   %i      the Icon key as "--icon VAL"  -> two arguments, or none
//   %c      the translated Name           -> one argument
//   %k      the location of the file      -> one argument
//   %%      a literal percent
//   %d %D %n %N %v %m  deprecated         -> removed

import (
	"errors"
	"fmt"
	"strings"
)

var errNoExec = errors.New("empty Exec")

// execWarning describes a spec violation found while tokenizing. It is
// informational: parsing continues, spec-correctly.
type execWarning struct {
	chars string // the unquoted reserved characters that were seen
}

// parseExec tokenizes an already value-unescaped Exec string and expands its
// field codes. iconVal and nameVal feed %i and %c; path feeds %k.
//
// The returned argv is safe to hand straight to execve: it never contains a
// shell, and no element is re-interpreted by anything.
func parseExec(execVal, path, nameVal, iconVal string) (argv []string, warn *execWarning, err error) {
	toks, reserved, terr := tokenizeExec(execVal)
	if terr != nil {
		return nil, nil, terr
	}
	if len(toks) == 0 {
		return nil, nil, errNoExec
	}
	if reserved != "" {
		warn = &execWarning{chars: reserved}
	}

	for _, t := range toks {
		expanded, drop := expandFieldCodes(t.text, t.quoted, path, nameVal, iconVal)
		if drop {
			continue
		}
		argv = append(argv, expanded...)
	}
	if len(argv) == 0 {
		return nil, warn, errNoExec
	}
	return argv, warn, nil
}

type execToken struct {
	text   string
	quoted bool // the argument was (at least partly) inside double quotes
}

// tokenizeExec splits an Exec value into arguments. It returns the set of
// unquoted reserved characters it saw, so the caller can report an entry
// that is relying on shell behaviour it will not get.
func tokenizeExec(s string) ([]execToken, string, error) {
	var (
		toks    []execToken
		cur     strings.Builder
		started bool
		quoted  bool
		seen    = map[byte]bool{}
	)
	flush := func() {
		if started {
			toks = append(toks, execToken{text: cur.String(), quoted: quoted})
			cur.Reset()
			started, quoted = false, false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '"':
			started, quoted = true, true
			// Consume up to the matching close quote, honouring the four
			// escapes the spec defines inside quotes.
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					n := s[i+1]
					if n == '"' || n == '`' || n == '$' || n == '\\' {
						cur.WriteByte(n)
						i += 2
						continue
					}
					// An undefined escape inside quotes: the spec does not
					// define it, so keep the backslash literally rather
					// than inventing a meaning.
					cur.WriteByte('\\')
					i++
					continue
				}
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, "", fmt.Errorf("unterminated quote in Exec")
			}
		case c == '\\':
			// Outside quotes the spec only allows a backslash as part of an
			// escape; a lone one is a reserved character used unquoted.
			if i+1 < len(s) {
				seen['\\'] = true
				i++
				cur.WriteByte(s[i])
				started = true
				continue
			}
			seen['\\'] = true
		default:
			if strings.IndexByte("'`><~|&;$*?#()", c) >= 0 {
				seen[c] = true
			}
			cur.WriteByte(c)
			started = true
		}
	}
	flush()

	var rs []byte
	for _, c := range []byte("'`><~|&;$*?#()\\") {
		if seen[c] {
			rs = append(rs, c)
		}
	}
	return toks, string(rs), nil
}

// expandFieldCodes turns one tokenized argument into zero or more real
// arguments. drop reports that the argument disappeared entirely, which is
// what "%f should be removed" means for an argument that was only a field
// code.
func expandFieldCodes(arg string, quoted bool, path, nameVal, iconVal string) (out []string, drop bool) {
	if !strings.ContainsRune(arg, '%') {
		return []string{arg}, false
	}
	var b strings.Builder
	var pre []string // arguments emitted before this one (%i)
	sawCode := false
	wroteText := false

	for i := 0; i < len(arg); i++ {
		if arg[i] != '%' {
			b.WriteByte(arg[i])
			wroteText = true
			continue
		}
		if i+1 >= len(arg) {
			// A trailing lone '%' is not a valid field code. The spec says
			// unrecognised codes are an error; be lenient and drop it.
			break
		}
		i++
		switch arg[i] {
		case '%':
			b.WriteByte('%')
			wroteText = true
		case 'f', 'F', 'u', 'U':
			// No files or URLs are being opened, so these are removed.
			sawCode = true
		case 'd', 'D', 'n', 'N', 'v', 'm':
			// Deprecated: "must be removed" and not substituted.
			sawCode = true
		case 'i':
			sawCode = true
			if iconVal != "" {
				pre = append(pre, "--icon", iconVal)
			}
		case 'c':
			sawCode = true
			b.WriteString(nameVal)
			if nameVal != "" {
				wroteText = true
			}
		case 'k':
			sawCode = true
			b.WriteString(path)
			if path != "" {
				wroteText = true
			}
		default:
			// Unknown field code: remove it rather than passing it through,
			// so an application never receives a literal "%z".
			sawCode = true
		}
	}
	text := b.String()
	if sawCode && !wroteText && strings.TrimSpace(text) == "" {
		if len(pre) > 0 {
			return pre, false
		}
		return nil, true
	}
	return append(pre, text), false
}
