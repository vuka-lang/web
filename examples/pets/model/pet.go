// Package model holds the pets app's data types, shared by its handlers and
// its views.
package model

type Pet struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type NewPet struct {
	Name string `json:"name" validate:"required"`
	Kind string `json:"kind"`
}
