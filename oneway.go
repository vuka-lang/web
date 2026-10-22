package web

import (
	"reflect"
	"strings"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/vuka/diag"
)

// A page is a struct that renders, in a .vuka.html file under pages/,
// which vuka registers in pages/route.go (web.Routes.Get/Live with
// PageRoute). A decorator declaring one — @web.Live, a route decorator on a
// type, or one on the constructor of a struct that renders — is E0720, which the compiler reports for the first two; this is
// the same check at Build, for code written against the registry by hand.
// A route registered from Go (app.Get, web.Routes) is the app's to choose:
// vukavet reports a page registered so by hand (E0720 too).

// decoratedPages are the errors of the registry's pages declared by a
// decorator.
func (a *App) decoratedPages() []error {
	var errs []error
	for _, r := range a.reg.routes {
		if r.opts != nil || !a.take(r.decl.Pkg) {
			continue
		}
		if why := renders(r.decl); why != "" {
			errs = append(errs, pageErr("@web."+verbName(r.method)+" on "+r.decl.Name+" ("+why+")", r.path).
				At(diag.Span{File: r.decl.File, Line: r.decl.Line}))
		}
	}
	for _, ls := range a.reg.lives {
		if !ls.registered && !ls.own && a.take(ls.pkg) {
			errs = append(errs, pageErr("@web.Live on "+ls.name, ls.path))
		}
	}
	for _, p := range a.reg.pages {
		if a.take(p.typ.Reflect.PkgPath()) {
			errs = append(errs, pageErr("@web."+verbName(p.method)+" on "+p.typ.Name, p.path))
		}
	}
	return errs
}

func pageErr(what, path string) *diag.Diagnostic {
	return diag.New("E0720", "%s declares a page outside pages/: a page is a .vuka.html file under pages/, its path its URL, and vuka registers it", what).
		WithHelp("move it to %s, the struct above its markup", pageFile(path))
}

// renders says what makes a decorated function a page: it constructs a
// struct that renders (a ui.Node, a live component, an island); "" for a
// handler, whatever it returns — a function answering with a ui.Node it
// builds is a handler too.
func renders(d *vuka.Decl) string {
	if t := pageCtor(d); t != nil {
		if p := reflect.PointerTo(t); p.Implements(nodeType) || p.Implements(statefulType) || ui.IsIsland(t) {
			return "its struct " + t.Name() + " renders"
		}
	}
	return ""
}

// verbName is a route decorator's name for its method: GET is Get.
func verbName(method string) string {
	if method == "" {
		return "Handle"
	}
	return method[:1] + strings.ToLower(method[1:])
}

// pageFile is the file under pages/ serving path: / is
// pages/index.vuka.html, /pets/{id} pages/pets/[id].vuka.html.
func pageFile(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "[" + s[1:len(s)-1] + "]"
		}
	}
	if len(segs) == 1 && segs[0] == "" {
		segs = []string{"index"}
	}
	return "pages/" + strings.Join(segs, "/") + ".vuka.html"
}
