package main

import "embed"

//go:embed all:dist
var adminFS embed.FS
