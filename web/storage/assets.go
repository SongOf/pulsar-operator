// Package web embeds the storage console so the API binary serves the same UI
// in standalone and gateway deployments without a Python runtime.
package web

import "embed"

// FS contains index.html, app.js and styles.css below the static directory.
//
//go:embed static/*
var FS embed.FS
