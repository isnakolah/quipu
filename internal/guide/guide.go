// Package guide holds the canonical agent handbook. `quipu guide` renders it
// with the guard and error tables, and generates every copy agents read.
package guide

import _ "embed"

// Agent is the handbook template. {{guard-rules}} and {{flow-errors}} are
// replaced with tables generated from the CLI.
//
//go:embed agent.md
var Agent string
