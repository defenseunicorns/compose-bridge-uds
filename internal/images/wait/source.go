// Package wait embeds the standalone TCP wait image's build context.
package wait

import _ "embed"

// Source is the complete, standard-library-only command source. Renderers can
// write it as main.go and build it without any other source files.
//
//go:embed cmd/main.go
var Source []byte

// Dockerfile builds the standalone command as a non-root, single-binary image.
//
//go:embed Dockerfile
var Dockerfile []byte
