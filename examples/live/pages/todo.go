package pages

type Item struct {
	ID   int
	Text string
	Done bool
}

type NewItem struct {
	Text string `form:"text"`
}
