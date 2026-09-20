// Package web embeds the server-rendered templates and static assets so the
// runtime image only needs the binary.
package web

import "embed"

//go:embed templates/*.html static/css/*.css static/js/*.js
var FS embed.FS
