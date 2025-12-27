package main

import _ "embed"

//go:embed web/index.html
var indexHTML string

//go:embed web/login.html
var loginHTML string

//go:embed web/style.css
var styleCSS string

//go:embed web/app.js
var appJS string

//go:embed web/icons.js
var iconsJS string

//go:embed web/alpine.min.js
var alpineJS string

//go:embed web/favicon.svg
var faviconSVG string
