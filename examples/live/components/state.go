package components

// Filters is a struct state: each field changes on its own.
type Filters struct {
	Kind string
	Sort string
}

type pet struct{ Name, Kind string }

var pets = []pet{{"Rex", "dog"}, {"Tom", "cat"}, {"Fido", "dog"}}
