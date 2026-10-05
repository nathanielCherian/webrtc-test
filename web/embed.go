// Package web embeds the browser client so the server binary is self-contained.
package web

import "embed"

//go:embed index.html app.js style.css common.js uplink.html uplink.js
var FS embed.FS
