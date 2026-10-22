package web

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/vuka/v2/diag"
)

// envSandbox moves the test into a fresh module directory holding files. The
// env file loads are forgotten, and every variable set meanwhile is removed
// when the test ends.
func envSandbox(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module sandbox\n"
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	before := map[string]bool{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		before[k] = true
	}
	forget := func() {
		envState.Lock()
		envState.loads = nil
		envState.Unlock()
		takeEnvErrors()
	}
	forget()
	t.Cleanup(func() {
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); !before[k] {
				os.Unsetenv(k)
			}
		}
		forget()
	})
	return dir
}

func TestParseDotEnv(t *testing.T) {
	env := map[string]string{"HOME": "/home/me", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	for _, c := range []struct{ name, src, want, err string }{
		{"plain", "A=1\nB=two words", "A=1|B=two words", ""},
		{"export", "export A=1\nexport\tB=2", "A=1|B=2", ""},
		{"comments and blanks", "# top\n\n  # indented\nA=1 # note\nB=x#y\nC= #only\nD=#also", "A=1|B=x#y|C=|D=", ""},
		{"spacing", "  A  =   hello world  \t\nB=", "A=hello world|B=", ""},
		{"crlf and bom", "\xef\xbb\xbfA=1\r\nB='2'\r\n", "A=1|B=2", ""},
		{"single quotes are literal", `A='$HOME \n ${X} # no'`, `A=$HOME \n ${X} # no`, ""},
		{"backticks are literal", "A=`it's \"$HOME\"`", `A=it's "$HOME"`, ""},
		{"double quote escapes", `A="a\nb\tc\"q\" \\ \$HOME \z" # c`, "A=a\nb\tc\"q\" \\ $HOME \\z", ""},
		{"multi-line", "A=\"one\ntwo\"\nB='three\nfour'\nC=5", "A=one\ntwo|B=three\nfour|C=5", ""},
		{"dotted keys", "a.b=1\nC=${a.b}", "a.b=1|C=1", ""},
		{"expansion", "A=x\nB=${A}y\nC=$A.z\nD=\"$HOME/${A}\"\nE='$A'", "A=x|B=xy|C=x.z|D=/home/me/x|E=$A", ""},
		{"defaults", "A=x\nB=${NOPE:-d}\nC=${A:-d}\nD=${EMPTY:-d}\nE=${NOPE:-${A}-2}\nF=\"${NOPE:-a b}\"", "A=x|B=d|C=x|D=d|E=x-2|F=a b", ""},
		{"undefined is empty", "A=[$NOPE][${NOPE}]", "A=[][]", ""},
		{"no cycles: only keys defined above", "A=${B}\nB=${A}", "A=|B=", ""},
		{"later wins, self reference", "A=1\nA=${A}2\nB=$A", "A=1|A=12|B=12", ""},
		{"the process wins", "HOME=/x\nB=$HOME", "HOME=/x|B=/home/me", ""},
		{"a lone dollar", "A=$ 5 $1 $", "A=$ 5 $1 $", ""},
		{"no key", "A=1\n=2", "", "expected KEY=value\n --> f.env:2"},
		{"bad key", "1A=x", "", "expected KEY=value\n --> f.env:1"},
		{"no equals", "A=1\n\nexport B", "", "expected KEY=value\n --> f.env:3"},
		{"unterminated single", "A=1\nB='x\ny", "", "unterminated '-quoted value of B\n --> f.env:2"},
		{"unterminated double", `A="x`, "", "unterminated double-quoted value of A\n --> f.env:1"},
		{"text after quote", `A="x" y`, "", "unexpected text after the closing quote\n --> f.env:1"},
		{"line after a multi-line value", "A=\"1\n2\"\nB='3\n4'\nnope", "", "expected KEY=value\n --> f.env:5"},
		{"unterminated brace", "A=${B", "", "unterminated ${\n --> f.env:1"},
		{"bad reference", "A=${1B}", "", "bad variable reference ${1B}\n --> f.env:1"},
	} {
		pairs, err := parseDotEnv("f.env", []byte(c.src), lookup, nil)
		if c.err != "" {
			if err == nil || diag.Of(err) != "E0416" || !strings.Contains(err.Error(), "error[E0416]: "+c.err) {
				t.Errorf("%s: err %v; want %q", c.name, err, c.err)
			}
			continue
		}
		var got []string
		for _, p := range pairs {
			got = append(got, p.key+"="+p.value)
		}
		if err != nil || strings.Join(got, "|") != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, strings.Join(got, "|"), err, c.want)
		}
	}
}

func TestParseDotEnvHidesValues(t *testing.T) {
	_, err := parseDotEnv(".env", []byte(`TOKEN="s3cr3t`), os.LookupEnv, nil)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("err %v: should fail without the value", err)
	}
}

func TestEnvPrecedence(t *testing.T) {
	envSandbox(t, map[string]string{
		".env":               "A=env\nB=env\nC=env\nD=env\nE=env\n",
		".env.staging":       "B=staging\nC=staging\nD=staging\n",
		".env.local":         "C=local\nD=local\nURL=${A}/${B}\n",
		".env.staging.local": "D=staging.local\n",
	})
	t.Setenv("VUKA_ENV", "staging")
	t.Setenv("E", "real")
	if err := LoadEnv(); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"A": "env", "B": "staging", "C": "local", "D": "staging.local", "E": "real", "URL": "env/staging"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q; want %q", k, got, want)
		}
	}
}

func TestEnvTestSkipsLocal(t *testing.T) {
	envSandbox(t, map[string]string{".env": "A=env\nB=env", ".env.local": "A=local", ".env.test": "B=test", ".env.test.local": "B=testlocal"})
	t.Setenv("VUKA_ENV", "") // go test: the "test" environment
	a := New(Only(pkgOf(t)))
	if err := a.Build(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("A") != "env" || os.Getenv("B") != "test" {
		t.Errorf("A=%q B=%q; .env.local files are for development, not tests", os.Getenv("A"), os.Getenv("B"))
	}
	if got := defaultEnvFiles(""); !reflect.DeepEqual(got, []string{".env.local", ".env"}) {
		t.Errorf("no environment: %v", got)
	}
}

func TestEnvModuleRoot(t *testing.T) {
	dir := envSandbox(t, map[string]string{".env": "ROOT=1", "cmd/app/main.go": "package main", "other/.env.test": "OTHER=1"})
	t.Chdir(filepath.Join(dir, "cmd", "app"))
	if err := LoadEnv(); err != nil || os.Getenv("ROOT") != "1" {
		t.Fatalf("from a package directory: ROOT=%q, %v", os.Getenv("ROOT"), err)
	}
	t.Chdir(filepath.Join(dir, "other"))
	if err := LoadEnv(); err != nil || os.Getenv("OTHER") != "1" {
		t.Fatalf("the working directory first: OTHER=%q, %v", os.Getenv("OTHER"), err)
	}
}

type envDB struct{ url, started string }

func (d *envDB) Start(context.Context) error {
	d.started = os.Getenv("DATABASE_URL_REPLICA")
	return nil
}

func TestEnvBeforeProviders(t *testing.T) {
	envSandbox(t, map[string]string{".env": "DATABASE_URL=postgres://localhost/pets\nDATABASE_URL_REPLICA=${DATABASE_URL}?replica=1\n"})
	a := New(Only(pkgOf(t)), Provide(func() *envDB { return &envDB{url: os.Getenv("DATABASE_URL")} }))
	d, err := Resolve[*envDB](a)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.url != "postgres://localhost/pets" || d.started != "postgres://localhost/pets?replica=1" {
		t.Errorf("constructor saw %q, Start saw %q", d.url, d.started)
	}
}

func TestLoadEnvThenNew(t *testing.T) {
	envSandbox(t, map[string]string{".env": "PORT=9090"})
	if err := LoadEnv(); err != nil {
		t.Fatal(err)
	}
	port := Env[int]("PORT")
	os.Setenv("PORT", "1") // a value set in code after the load stays
	if err := New(Only(pkgOf(t))).Build(); err != nil || port != 9090 || os.Getenv("PORT") != "1" {
		t.Errorf("port %d, PORT=%q, %v", port, os.Getenv("PORT"), err)
	}
}

func TestDotEnvOptions(t *testing.T) {
	envSandbox(t, map[string]string{".env": "A=default", "one.env": "B=one", "two.env": "B=two\nC=two"})
	if err := New(Only(pkgOf(t)), NoDotEnv()).Build(); err != nil || os.Getenv("A") != "" {
		t.Fatalf("NoDotEnv: A=%q, %v", os.Getenv("A"), err)
	}
	if err := New(Only(pkgOf(t)), DotEnv("one.env", "two.env", "missing.env")).Build(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("A") != "" || os.Getenv("B") != "one" || os.Getenv("C") != "two" {
		t.Errorf("DotEnv: A=%q B=%q C=%q", os.Getenv("A"), os.Getenv("B"), os.Getenv("C"))
	}
}

func TestEnvFileErrors(t *testing.T) {
	envSandbox(t, map[string]string{".env": "GOOD=1\nPASSWORD='hunter2\n", ".env.test": "FINE=1"})
	err := New(Only(pkgOf(t))).Build()
	if err == nil || !strings.Contains(err.Error(), "error[E0416]: unterminated '-quoted value of PASSWORD\n --> .env:2") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("Build: %v", err)
	}
	if os.Getenv("GOOD") != "" || os.Getenv("FINE") != "1" {
		t.Errorf("a broken file sets nothing, the others still apply: GOOD=%q FINE=%q", os.Getenv("GOOD"), os.Getenv("FINE"))
	}
}

func TestRequire(t *testing.T) {
	envSandbox(t, map[string]string{".env": "SET=x\nEMPTY=\n", ".env.example": "SET=\nSECRET_KEY=\nSMTP_HOST=\n"})
	err := New(Only(pkgOf(t)), Require("SET", "EMPTY", "API_TOKEN"), Require("API_TOKEN")).Build()
	want := "error[E0415]: missing environment variables: EMPTY, API_TOKEN (read .env; looked for .env.test, .env); also unset, listed in .env.example: SECRET_KEY, SMTP_HOST\n  = help: set them, or put them in .env"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Build:\n%v\nwant\n%s", err, want)
	}
	t.Setenv("EMPTY", "y")
	t.Setenv("API_TOKEN", "z")
	if err := New(Only(pkgOf(t)), Require("SET", "EMPTY", "API_TOKEN")).Build(); err != nil {
		t.Error(err)
	}
	err = New(Only(pkgOf(t)), NoDotEnv(), Require("NOPE")).Build()
	if err == nil || !strings.Contains(err.Error(), "NOPE (no env files are read)") {
		t.Errorf("NoDotEnv: %v", err)
	}
}

type level string

func (l *level) UnmarshalText(b []byte) error {
	if s := string(b); s == "debug" || s == "info" {
		*l = level(s)
		return nil
	}
	return os.ErrInvalid
}

func TestEnvConversions(t *testing.T) {
	envSandbox(t, map[string]string{})
	for k, v := range map[string]string{"S": "hi", "B": "Yes", "I": "-42", "U": "7", "F": "2.5", "D": "1m30s",
		"L": " a, b ,,c ", "N": "1,2,3", "URL": "postgres://u:p@h/db", "IP": "10.0.0.1", "LVL": "debug"} {
		t.Setenv(k, v)
	}
	check := func(name string, got, want any) {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %#v; want %#v", name, got, want)
		}
	}
	check("string", Env[string]("S"), "hi")
	check("bool", Env[bool]("B"), true)
	check("int", Env[int]("I"), -42)
	check("uint8", Env[uint8]("U"), uint8(7))
	check("float", Env[float64]("F"), 2.5)
	check("duration", Env[time.Duration]("D"), 90*time.Second)
	check("[]string", Env[[]string]("L"), []string{"a", "b", "c"})
	check("[]int", Env[[]int]("N"), []int{1, 2, 3})
	check("url", Env[*url.URL]("URL").Host, "h")
	check("text", Env[netip.Addr]("IP"), netip.MustParseAddr("10.0.0.1"))
	check("named text", Env[level]("LVL"), level("debug"))
	check("unset", Env[int]("UNSET"), 0)
	check("default", Env("UNSET", 8080), 8080)
	check("default duration", Env("UNSET", time.Second), time.Second)
	if err := New(Only(pkgOf(t))).Build(); err != nil {
		t.Fatalf("no conversion errors yet: %v", err)
	}

	t.Setenv("PORT", "eighty")
	t.Setenv("DEBUG", "maybe")
	t.Setenv("DSN", "s3cr3t-no-scheme")
	check("bad int", Env("PORT", 8080), 8080)
	check("bad bool", Env[bool]("DEBUG"), false)
	check("bad url", Env[*url.URL]("DSN") == nil, true)
	check("overflow", Env[int8]("U"), int8(7))
	t.Setenv("U", "300")
	check("overflow", Env[int8]("U", 1), int8(1))
	err := New(Only(pkgOf(t))).Build()
	for _, want := range []string{"PORT: not a valid int", "DEBUG: not a boolean", "DSN: not an absolute URL", "U: not a valid int8"} {
		if err == nil || !strings.Contains(err.Error(), "environment variable "+want) {
			t.Errorf("Build: %v; want %q", err, want)
		}
	}
	if err != nil && (strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "eighty")) {
		t.Errorf("Build shows a value: %v", err)
	}
	if err := New(Only(pkgOf(t))).Build(); err != nil {
		t.Errorf("errors are reported once: %v", err)
	}

	a := New(Only(pkgOf(t)), Provide(func() *envDB { return &envDB{url: Env("PORT", "x")} }, func() int { return Env[int]("PORT") }))
	if _, err := Resolve[int](a); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "PORT: not a valid int") {
		t.Errorf("Start reports a constructor's conversion error: %v", err)
	}
}

func TestMustEnv(t *testing.T) {
	envSandbox(t, map[string]string{})
	t.Setenv("N", "3")
	t.Setenv("BAD", "x")
	if MustEnv[int]("N") != 3 {
		t.Error("MustEnv")
	}
	for _, k := range []string{"UNSET", "BAD"} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), k) || strings.Contains(fmt.Sprint(r), "x\"") {
					t.Errorf("MustEnv(%s): %v", k, r)
				}
			}()
			MustEnv[int](k)
		}()
	}
}

func TestEnvDevLog(t *testing.T) {
	envSandbox(t, map[string]string{".env": "A=1", ".env.development": "TOKEN=s3cr3t"})
	t.Setenv("VUKA_ENV", "development")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	a := New(Only(pkgOf(t)))
	New(Only(pkgOf(t)))
	if !a.dev || strings.Count(buf.String(), "web: environment from .env.development, .env\n") != 1 || strings.Contains(buf.String(), "s3cr3t") {
		t.Errorf("dev %v, log %q", a.dev, buf.String())
	}
}
