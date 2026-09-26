package render

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
)

// referenceIntro heads docs/configuration.md.
const referenceIntro = `<!-- Generated from internal/render by ` + "`make docs`" + `; edit the struct tags there, not this file. -->

# Configuration

Every setting is an environment variable. ` + "`otlp-collector-oidc run`" + ` (the image's
command) reads them, renders the shipped pipeline and starts the collector on it;
the rendered file is written to ` + "`" + RenderedFile + "`" + ` in the temporary directory
(` + "`/tmp`" + ` in the image) and logged at ` + "`LOG_LEVEL=debug`" + `.

A variable set to the empty string counts as unset. A **required** variable that is
unset, or any value that does not parse, stops the process before it listens, and the
error names the variable and the value.

A provider or upstream whose certificate is signed by a private CA is trusted the
standard Go way: mount the CA and set ` + "`SSL_CERT_FILE`" + ` or ` + "`SSL_CERT_DIR`" + `.
`

// codeSpan matches 'x' where x has no spaces or quotes: the doc tags' way of
// writing code, since a struct tag cannot hold a backtick.
var codeSpan = regexp.MustCompile(`(^|[\s(])'([^'\s]+)'`)

// Reference is docs/configuration.md, generated from the field tags of
// Settings and Source.
func Reference() []byte {
	var b bytes.Buffer
	b.WriteString(referenceIntro)
	groups := append(groupsOf(reflect.ValueOf(&Settings{}).Elem()), groupsOf(reflect.ValueOf(&Source{}).Elem())...)
	for _, g := range groups {
		fmt.Fprintf(&b, "\n## %s\n\n| Variable | Default | Meaning |\n| --- | --- | --- |\n", g.title)
		for _, v := range g.variables {
			def := "*(empty)*"
			switch {
			case v.required:
				def = "**required**"
			case v.def != "":
				def = "`" + v.def + "`"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", v.name, def, codeSpan.ReplaceAllString(v.doc, "$1`$2`"))
		}
		if g.note != "" {
			fmt.Fprintf(&b, "\n%s\n", g.note)
		}
	}
	return b.Bytes()
}
