package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
)

func TestRegistrationMistakes(t *testing.T) {
	a := New(Only("none")).
		Get("/x", 42).
		Live("/y", func() int { return 1 }).
		Service(func() {})
	err := a.Build()
	for _, want := range []string{"GET /x: the handler is int, not a function", "Live(\"/y\", func() int)", "Service(func())"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Build: %v; want %q", err, want)
		}
	}
}

// islandOpt is a type decorator as a route's option, as ui.Island is.
type islandOpt func(*vuka.Type)

func (o islandOpt) Attrs() []any { return []any{o} }

type regApplicants struct {
	Q    string   `json:"q" url:"q"`
	Rows []string `json:"rows"`
	db   *applicantDB
}

func newRegApplicants(db *applicantDB) *regApplicants {
	return &regApplicants{Rows: db.find(""), db: db}
}

type regLiveApplicants struct {
	ui.Live
	Rows []string `json:"rows"`
	db   *applicantDB
}

func newRegLiveApplicants() *regLiveApplicants {
	return &regLiveApplicants{Rows: []string{"from the constructor"}}
}

func (a *regLiveApplicants) Render() ui.Node { return ui.IslandPage(a) }

func TestRegisteredIslandPages(t *testing.T) {
	db := &applicantDB{names: []string{"Ada", "Grace"}}
	h := New(Only("none"), Provide(func() *applicantDB { return db })).
		Get("/reg/applicants", newRegApplicants, islandOpt(ui.Island("components/Applicants.vue"))).
		Live("/reg/live/applicants", newRegLiveApplicants, islandOpt(ui.Island("components/Applicants.vue"))).
		Handler()
	if !ui.IsIsland(typeOf[regApplicants]()) || !ui.IsIsland(typeOf[regLiveApplicants]()) {
		t.Fatal("the island options didn't register the types")
	}
	code, body, _ := do(t, h, "GET", "/reg/applicants", "")
	if props, _, sig := rendered(t, body); code != 200 || len(props["rows"].([]any)) != 2 || sig == "" {
		t.Errorf("a stateless page: %d %v %q", code, props, sig)
	}
	code, body, _ = do(t, h, "GET", "/reg/live/applicants", "")
	if props, _, _ := rendered(t, body); code != 200 || fmt.Sprint(props["rows"]) != "[from the constructor]" {
		t.Errorf("a live page: %d %v", code, props)
	}
}
