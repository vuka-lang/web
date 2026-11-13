// Command minjs writes the minified runtime the web package serves outside
// development: live.min.js beside live.js. It is its own module, so esbuild
// is a dependency of this tool only, never of web or the apps built on it.
//
//	cd internal/minjs && go run . ../../live.js ../../live.min.js
package main

import (
	"fmt"
	"os"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: minjs <in.js> <out.js>")
		os.Exit(2)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := Minify(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Minify is src minified: whitespace, identifiers and syntax, the language
// left as written (no lowering), non-ASCII kept as is.
func Minify(src []byte) ([]byte, error) {
	r := api.Transform(string(src), api.TransformOptions{
		Loader:            api.LoaderJS,
		Target:            api.ESNext,
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		Charset:           api.CharsetUTF8,
		LegalComments:     api.LegalCommentsNone,
	})
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("minjs: %s", r.Errors[0].Text)
	}
	return r.Code, nil
}
