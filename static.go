package main

import _ "embed"

// indexHTML is the embedded main application HTML template.
//go:embed web/index.html
var indexHTML string

// loginHTML is the embedded login page HTML template.
//go:embed web/login.html
var loginHTML string

// styleCSS is the embedded CSS stylesheet.
//go:embed web/style.css
var styleCSS string

// appJS is the embedded JavaScript application code.
//go:embed web/app.js
var appJS string

// iconsJS is the embedded SVG icons module.
//go:embed web/icons.js
var iconsJS string

// alpineJS is the embedded Alpine.js framework.
//go:embed web/alpine.min.js
var alpineJS string

// faviconSVG is the embedded favicon SVG template.
//go:embed web/favicon.svg
var faviconSVG string
