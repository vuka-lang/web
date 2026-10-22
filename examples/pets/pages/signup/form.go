// Package signup is /signup: the form, its POST, and the page after it.
package signup

// Form is the sign-up form.
type Form struct {
	Name  string `form:"name" validate:"required"`
	Email string `form:"email" validate:"required"`
}
