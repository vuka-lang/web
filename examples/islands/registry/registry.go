// Package registry is the applicants' store, provided to the app.
package registry

type Applicant struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type Registry struct{ All []Applicant }

func New() *Registry {
	return &Registry{All: []Applicant{{"Ada", "engineer"}, {"Grace", "admiral"}, {"Alan", "mathematician"}}}
}
