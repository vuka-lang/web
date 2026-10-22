// Package library is the books the shelf and the book pages show.
package library

import "fmt"

// Book is a book of the shelf.
type Book struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author"`
}

var Books = []Book{
	{1, "Things Fall Apart", "Chinua Achebe"}, {2, "Petals of Blood", "Ngũgĩ wa Thiong'o"},
	{3, "Half of a Yellow Sun", "Chimamanda Ngozi Adichie"}, {4, "Season of Migration to the North", "Tayeb Salih"},
	{5, "The Famished Road", "Ben Okri"}, {6, "Paradise", "Abdulrazak Gurnah"},
	{7, "Nervous Conditions", "Tsitsi Dangarembga"}, {8, "So Long a Letter", "Mariama Bâ"},
	{9, "Weep Not, Child", "Ngũgĩ wa Thiong'o"}, {10, "The Beautyful Ones Are Not Yet Born", "Ayi Kwei Armah"},
	{11, "Americanah", "Chimamanda Ngozi Adichie"}, {12, "By the Sea", "Abdulrazak Gurnah"},
	{13, "Disgrace", "J. M. Coetzee"}, {14, "The Joys of Motherhood", "Buchi Emecheta"},
	{15, "Arrow of God", "Chinua Achebe"}, {16, "Kintu", "Jennifer Nansubuga Makumbi"},
	{17, "Afterlives", "Abdulrazak Gurnah"}, {18, "The Promise", "Damon Galgut"},
}

const PerPage = 5

// Pages is the number of the shelf's pages.
func Pages() int { return (len(Books) + PerPage - 1) / PerPage }

// Get is the book id, the nearest one for an id out of range.
func Get(id int) Book { return Books[min(max(id, 1), len(Books))-1] }

func ShelfURL(page int) string { return fmt.Sprint("/books?page=", page) }

func BookURL(id int) string { return fmt.Sprint("/books/", id) }
