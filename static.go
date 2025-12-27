// Package main embeds static web assets (HTML, CSS, JavaScript) directly into
// the compiled binary using Go's embed directive. This eliminates external file
// dependencies and simplifies deployment to a single executable.
package main

import _ "embed"

// indexHTML contains the embedded index.html template.
//
//go:embed web/index.html
var indexHTML string

// loginHTML contains the embedded login.html template.
//
//go:embed web/login.html
var loginHTML string

// styleCSS contains the embedded stylesheet.
//
//go:embed web/style.css
var styleCSS string

// appJS contains the embedded application JavaScript.
//
//go:embed web/app.js
var appJS string

// iconsJS contains the embedded icon definitions.
//
//go:embed web/icons.js
var iconsJS string

// alpineJS contains the embedded Alpine.js framework.
//
//go:embed web/alpine.min.js
var alpineJS string

// faviconSVG contains the embedded favicon.
//
//go:embed web/favicon.svg
var faviconSVG string
