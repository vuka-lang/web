package web

import (
	"encoding"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// envLoad is one reading of a list of env files from one directory.
type envLoad struct {
	dir      string
	searched []string // every candidate, highest precedence first
	loaded   []string // those found
	err      error
}

var envState struct {
	sync.Mutex
	loads map[string]*envLoad // by working directory and file list
	errs  []error             // Env conversion errors not yet reported by a Build
}

// envName is the environment that picks the env files: $VUKA_ENV, else
// "test" under go test, else none.
func envName() string {
	if v := os.Getenv("VUKA_ENV"); v != "" {
		return v
	}
	if testing.Testing() {
		return "test"
	}
	return ""
}

// defaultEnvFiles lists the env files of environment env, the first winning.
func defaultEnvFiles(env string) []string {
	switch env {
	case "":
		return []string{".env.local", ".env"}
	case "test":
		return []string{".env.test", ".env"}
	}
	return []string{".env." + env + ".local", ".env.local", ".env." + env, ".env"}
}

// envDir is where relative env files are read: the working directory, or
// the nearest of its parents, up to the main module's root (the directory
// holding go.mod), that has one of them. Without a go.mod in the working
// directory or above it, only the working directory: a deployed binary
// never reads an env file of a directory above its own.
func envDir(cwd string, files []string) string {
	root := cwd
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			root = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		for _, f := range files {
			if !filepath.IsAbs(f) {
				if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
					return dir
				}
			}
		}
		if dir == root || filepath.Dir(dir) == dir {
			return cwd
		}
	}
}

// reservedEnv reports whether key is web's own (VUKA_ENV, VUKA_DEV_DIR,
// VUKA_DEV_LIVE_KEY…): those switch an app into development, so only the
// real environment sets them, never an env file.
func reservedEnv(key string) bool { return strings.HasPrefix(key, "VUKA_") }

// IsDev reports whether the process runs in development: VUKA_ENV is
// "development", or vuka run names its session directory (VUKA_DEV_DIR).
// Both come from the real environment only — an env file can't set them —
// so a deployed binary is never switched into development by a file next to
// it. (An app made with the Dev option is in development whatever IsDev
// says.)
func IsDev() bool {
	return os.Getenv("VUKA_ENV") == "development" || os.Getenv(DevDirEnv) != ""
}

// LoadEnv reads env files into the process environment, as New does: for
// code that needs their values before New, such as options built from
// them. No files means the default list (see the README); a key is set only
// when the environment doesn't have it yet, and the first file listed wins
// over the later ones. Loading the same files again is a no-op, so New
// reading them afterwards changes nothing.
func LoadEnv(files ...string) error { return loadEnv(files).err }

func loadEnv(files []string) *envLoad {
	if files == nil {
		files = defaultEnvFiles(envName())
	}
	cwd, _ := os.Getwd()
	envState.Lock()
	defer envState.Unlock()
	key := cwd + "\x00" + strings.Join(files, "\x00")
	if l, ok := envState.loads[key]; ok {
		return l
	}
	l := &envLoad{dir: envDir(cwd, files)}
	if envState.loads == nil {
		envState.loads = map[string]*envLoad{}
	}
	envState.loads[key] = l
	vars := map[string]string{}
	var errs []error
	for i := len(files) - 1; i >= 0; i-- {
		path := files[i]
		if !filepath.IsAbs(path) {
			path = filepath.Join(l.dir, path)
		}
		name := path
		if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
			name = rel
		}
		l.searched = append([]string{name}, l.searched...)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("web: reading %s: %w", name, err))
			continue
		}
		pairs, err := parseDotEnv(name, data, os.LookupEnv, vars)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range pairs {
			if reservedEnv(p.key) {
				log.Printf("web: %s sets %s, which only the real environment may set: ignored", name, p.key)
				continue
			}
			vars[p.key] = p.value
		}
		l.loaded = append([]string{name}, l.loaded...)
	}
	for k, v := range vars {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	l.err = errors.Join(errs...)
	if len(l.loaded) > 0 && os.Getenv("VUKA_ENV") == "development" {
		log.Printf("web: environment from %s", strings.Join(l.loaded, ", "))
	}
	return l
}

// DotEnv replaces the env files New reads (see LoadEnv); the first listed
// wins. Relative paths are read from the working directory, or the nearest
// parent up to the module root holding one of them.
func DotEnv(files ...string) Option {
	return OptionFunc(func(a *App) { a.envFiles = append([]string{}, files...) })
}

// NoDotEnv keeps New from reading env files.
func NoDotEnv() Option { return DotEnv() }

// Require fails Build when one of the environment variables is unset or
// empty, naming each missing one and the env files looked for.
func Require(keys ...string) Option {
	return OptionFunc(func(a *App) { a.required = append(a.required, keys...) })
}

func (a *App) checkRequired() error {
	var missing []string
	for _, k := range a.required {
		if os.Getenv(k) == "" && !slices.Contains(missing, k) {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	msg := "web: missing environment variables: " + strings.Join(missing, ", ")
	switch l := a.env; {
	case l == nil || len(l.searched) == 0:
		msg += " (no env files are read)"
	case len(l.loaded) == 0:
		msg += fmt.Sprintf(" (looked for %s; none found)", strings.Join(l.searched, ", "))
	default:
		msg += fmt.Sprintf(" (read %s; looked for %s)", strings.Join(l.loaded, ", "), strings.Join(l.searched, ", "))
	}
	if a.env != nil {
		if data, err := os.ReadFile(filepath.Join(a.env.dir, ".env.example")); err == nil {
			pairs, _ := parseDotEnv(".env.example", data, func(string) (string, bool) { return "", true }, nil)
			var also []string
			for _, p := range pairs {
				if os.Getenv(p.key) == "" && !slices.Contains(missing, p.key) && !slices.Contains(also, p.key) {
					also = append(also, p.key)
				}
			}
			if len(also) > 0 {
				msg += "; also unset, listed in .env.example: " + strings.Join(also, ", ")
			}
		}
	}
	return errors.New(msg)
}

// Env returns the environment variable key as a T: a string, bool, integer,
// float, time.Duration, *url.URL (absolute), a type implementing
// encoding.TextUnmarshaler, or a slice of those (comma-separated). When key
// is unset or empty it returns def (or the zero value); when the value doesn't
// convert, it returns def too and the next Build or Start reports the error.
func Env[T any](key string, def ...T) T {
	v, set, err := lookupEnv[T](key)
	if err != nil {
		envState.Lock()
		envState.errs = append(envState.errs, err)
		envState.Unlock()
	}
	if set && err == nil {
		return v
	}
	if len(def) > 0 {
		return def[0]
	}
	var zero T
	return zero
}

// MustEnv is Env for values that must be there: it panics, naming key, when
// the variable is unset, empty or doesn't convert.
func MustEnv[T any](key string) T {
	v, set, err := lookupEnv[T](key)
	if err != nil {
		panic(err)
	}
	if !set {
		panic("web: environment variable " + key + " is not set")
	}
	return v
}

func takeEnvErrors() []error {
	envState.Lock()
	defer envState.Unlock()
	errs := envState.errs
	envState.errs = nil
	return errs
}

func lookupEnv[T any](key string) (T, bool, error) {
	var v T
	s := os.Getenv(key)
	if s == "" {
		return v, false, nil
	}
	if err := parseEnvValue(s, reflect.ValueOf(&v).Elem()); err != nil {
		return v, true, fmt.Errorf("web: environment variable %s: %w", key, err)
	}
	return v, true, nil
}

var (
	durationType = typeOf[time.Duration]()
	urlType      = typeOf[*url.URL]()
)

// parseEnvValue sets v from s. Its errors never quote s: env values are often
// secrets.
func parseEnvValue(s string, v reflect.Value) error {
	t := v.Type()
	bad := fmt.Errorf("not a valid %s", t)
	switch {
	case t == durationType:
		d, err := time.ParseDuration(s)
		if err != nil {
			return bad
		}
		v.SetInt(int64(d))
		return nil
	case t == urlType:
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" {
			return errors.New("not an absolute URL")
		}
		v.Set(reflect.ValueOf(u))
		return nil
	case reflect.PointerTo(t).Implements(textType):
		if v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)) != nil {
			return bad
		}
		return nil
	}
	switch t.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		switch strings.ToLower(s) {
		case "1", "t", "true", "yes", "y", "on":
			v.SetBool(true)
		case "0", "f", "false", "no", "n", "off":
			v.SetBool(false)
		default:
			return errors.New("not a boolean")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, t.Bits())
		if err != nil {
			return bad
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, t.Bits())
		if err != nil {
			return bad
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, t.Bits())
		if err != nil {
			return bad
		}
		v.SetFloat(n)
	case reflect.Slice:
		var items []string
		for _, it := range strings.Split(s, ",") {
			if it = strings.TrimSpace(it); it != "" {
				items = append(items, it)
			}
		}
		out := reflect.MakeSlice(t, len(items), len(items))
		for i, it := range items {
			if err := parseEnvValue(it, out.Index(i)); err != nil {
				return fmt.Errorf("item %d: %w", i+1, err)
			}
		}
		v.Set(out)
	default:
		return fmt.Errorf("Env can't read a %s", t)
	}
	return nil
}
