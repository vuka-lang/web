package web

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Starter is a provided value with work to do when the app starts: Run calls
// Start in dependency order.
type Starter interface{ Start(context.Context) error }

// Stopper is a provided value with work to do when the app stops: Stop is
// called in the reverse order of construction.
type Stopper interface{ Stop(context.Context) error }

// hook is one step of the app's lifecycle: a value's Start/Stop or a hook
// added with OnStart/OnStop.
type hook struct {
	name        string
	start, stop func(context.Context) error
}

func valueHook(name string, v reflect.Value) hook {
	h := hook{name: name}
	if s, ok := v.Interface().(Starter); ok {
		h.start = s.Start
	}
	if s, ok := v.Interface().(Stopper); ok {
		h.stop = s.Stop
	}
	return h
}

type provider struct {
	fn     reflect.Value // nil for a supplied value
	in     []reflect.Type
	out    reflect.Type
	hasErr bool
	source string
	value  reflect.Value
	built  bool
}

type container struct {
	mu        sync.Mutex
	providers map[reflect.Type]*provider
	hooks     []hook // supplied values and hooks as added, built values as built
	errs      []error
}

var errType = typeOf[error]()

func newContainer() *container { return &container{providers: map[reflect.Type]*provider{}} }

func (c *container) provide(ctor any, source string) *provider {
	v := reflect.ValueOf(ctor)
	if !v.IsValid() {
		c.errs = append(c.errs, fmt.Errorf("web: Provide(nil)"))
		return nil
	}
	p := &provider{source: source}
	if t := v.Type(); t.Kind() == reflect.Func {
		if t.NumOut() == 0 || t.NumOut() > 2 || (t.NumOut() == 2 && t.Out(1) != errType) || t.IsVariadic() {
			c.errs = append(c.errs, fmt.Errorf("web: constructor %s must be func(deps…) T or func(deps…) (T, error)", t))
			return nil
		}
		p.fn, p.out, p.hasErr = v, t.Out(0), t.NumOut() == 2
		for i := 0; i < t.NumIn(); i++ {
			p.in = append(p.in, t.In(i))
		}
		if p.source == "" {
			p.source = funcName(v)
		}
	} else {
		p.out, p.value, p.built = t, v, true
		if p.source == "" {
			p.source = "a provided " + t.String()
		}
	}
	if old, dup := c.providers[p.out]; dup {
		c.errs = append(c.errs, fmt.Errorf("web: %s is provided twice: by %s and by %s", p.out, old.source, p.source))
		return nil
	}
	c.providers[p.out] = p
	if p.built {
		c.hooks = append(c.hooks, valueHook(p.out.String(), p.value))
	}
	return p
}

func (c *container) has(t reflect.Type) bool { _, ok := c.providers[t]; return ok }

// check reports a missing provider or a cycle among t's dependencies; who is
// the one needing t.
func (c *container) check(t reflect.Type, who string) error {
	return c.walk(t, []string{who}, nil, map[reflect.Type]bool{})
}

func (c *container) walk(t reflect.Type, chain []string, path []reflect.Type, done map[reflect.Type]bool) error {
	for i, s := range path {
		if s == t {
			names := make([]string, 0, len(path)-i+1)
			for _, s := range path[i:] {
				names = append(names, s.String())
			}
			return fmt.Errorf("web: dependency cycle: %s -> %s", strings.Join(names, " -> "), t)
		}
	}
	if done[t] {
		return nil
	}
	p, ok := c.providers[t]
	if !ok {
		for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
			chain[i], chain[j] = chain[j], chain[i]
		}
		return fmt.Errorf("web: no provider for %s, needed by %s", t, strings.Join(chain, ", needed by "))
	}
	for _, in := range p.in {
		if err := c.walk(in, append(chain[:len(chain):len(chain)], p.source), append(path, t), done); err != nil {
			return err
		}
	}
	done[t] = true
	return nil
}

// get returns t's singleton, building it and its dependencies on first use.
func (c *container) get(t reflect.Type) (reflect.Value, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.build(t, nil)
}

func (c *container) build(t reflect.Type, path []reflect.Type) (reflect.Value, error) {
	p, ok := c.providers[t]
	if !ok {
		return reflect.Value{}, fmt.Errorf("web: no provider for %s", t)
	}
	if p.built {
		return p.value, nil
	}
	for _, s := range path {
		if s == t {
			return reflect.Value{}, fmt.Errorf("web: dependency cycle at %s", t)
		}
	}
	args := make([]reflect.Value, len(p.in))
	for i, in := range p.in {
		v, err := c.build(in, append(path, t))
		if err != nil {
			return reflect.Value{}, err
		}
		args[i] = v
	}
	out := p.fn.Call(args)
	if p.hasErr && !out[1].IsNil() {
		return reflect.Value{}, fmt.Errorf("web: %s: %w", p.source, out[1].Interface().(error))
	}
	p.value, p.built = out[0], true
	c.hooks = append(c.hooks, valueHook(p.out.String(), p.value))
	return p.value, nil
}

func (c *container) hook(h hook) {
	c.mu.Lock()
	c.hooks = append(c.hooks, h)
	c.mu.Unlock()
}

func (c *container) start(ctx context.Context) error {
	c.mu.Lock()
	hooks := slices.Clone(c.hooks)
	c.mu.Unlock()
	for _, h := range hooks {
		if h.start != nil {
			if err := h.start(ctx); err != nil {
				return fmt.Errorf("web: starting %s: %w", h.name, err)
			}
		}
	}
	return nil
}

func (c *container) stop(ctx context.Context) error {
	c.mu.Lock()
	hooks := slices.Clone(c.hooks)
	c.mu.Unlock()
	var errs []error
	for _, h := range slices.Backward(hooks) {
		if h.stop != nil {
			if err := h.stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("web: stopping %s: %w", h.name, err))
			}
		}
	}
	return errors.Join(errs...)
}
