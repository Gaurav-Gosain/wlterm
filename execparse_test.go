package main

import (
	"reflect"
	"testing"
)

func TestExecTokenizing(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", `firefox`, []string{"firefox"}},
		{"args", `code-oss --new-window`, []string{"code-oss", "--new-window"}},
		{"file code dropped", `gimp %F`, []string{"gimp"}},
		{"url code dropped", `firefox %u`, []string{"firefox"}},
		{"quoted path with space", `"/opt/My App/run" --flag`, []string{"/opt/My App/run", "--flag"}},
		{"escaped quote inside quotes", `sh "a\"b"`, []string{"sh", `a"b`}},
		{"escaped dollar inside quotes", `x "a\$b"`, []string{"x", `a$b`}},
		{"escaped backslash inside quotes", `x "a\\b"`, []string{"x", `a\b`}},
		{"percent literal", `x 100%%`, []string{"x", "100%"}},
		{"deprecated removed", `x %d %D %n %N %v %m y`, []string{"x", "y"}},
		{"embedded code in arg", `x --file=%f`, []string{"x", "--file="}},
		{"real world quoted arg with url template",
			`kde-geo-uri-handler --t "https://x/?a=<A>&b=<B>" %u`,
			[]string{"kde-geo-uri-handler", "--t", "https://x/?a=<A>&b=<B>"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := parseExec(c.in, "/p/x.desktop", "Name", "")
			if err != nil {
				t.Fatalf("parseExec(%q): %v", c.in, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseExec(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestExecFieldCodes(t *testing.T) {
	// %i expands to two arguments, or to none when there is no Icon.
	got, _, _ := parseExec(`app %i --x`, "/p/x.desktop", "The Name", "myicon")
	want := []string{"app", "--icon", "myicon", "--x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%%i with icon = %q, want %q", got, want)
	}
	got, _, _ = parseExec(`app %i --x`, "/p/x.desktop", "The Name", "")
	want = []string{"app", "--x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%%i without icon = %q, want %q", got, want)
	}
	// %c is the translated name, %k the file location.
	got, _, _ = parseExec(`app %c %k`, "/p/x.desktop", "The Name", "")
	want = []string{"app", "The Name", "/p/x.desktop"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%%c %%k = %q, want %q", got, want)
	}
}

// TestExecIsNotAShell is the security property: an Exec value is tokenized
// per the spec and exec'd as argv, so shell metacharacters have no meaning.
// A .desktop file can be dropped anywhere on the XDG search path, so this is
// the difference between a launcher and a remote-code-execution primitive.
func TestExecIsNotAShell(t *testing.T) {
	argv, warn, err := parseExec(`app; rm -rf ~ | tee /dev/null & echo $(whoami)`,
		"/p/x.desktop", "n", "")
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "app;" {
		t.Fatalf("expected the semicolon to stay glued to the literal token, got %q", argv)
	}
	for _, a := range argv {
		if a == "|" || a == "&" {
			// Present as literal argv elements, which is the point: nothing
			// will ever interpret them.
			continue
		}
	}
	if warn == nil {
		t.Fatal("expected a warning about unquoted reserved characters")
	}
	// And no element is a shell.
	for _, a := range argv {
		if a == "sh" || a == "/bin/sh" || a == "bash" {
			t.Fatalf("a shell appeared in argv: %q", argv)
		}
	}
}

func TestExecUnterminatedQuote(t *testing.T) {
	if _, _, err := parseExec(`app "unclosed`, "/p/x.desktop", "n", ""); err == nil {
		t.Fatal("expected an error for an unterminated quote")
	}
}

func TestExecOnlyFieldCodes(t *testing.T) {
	if _, _, err := parseExec(`%f`, "/p/x.desktop", "n", ""); err == nil {
		t.Fatal("an Exec that is only a removed field code has no program to run")
	}
}
