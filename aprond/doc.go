// Package aprond is the aprond command as a library. It wraps package apron
// in what a program needs to run it: command line flags and a TOML config
// file, listeners with automatic TLS (Let's Encrypt), SMTP delivery of email
// sign-in codes, an optional debug listener, and a graceful shutdown on
// SIGINT and SIGTERM.
//
// It is a library so that a server of your own is a few lines. A main package
// that calls [Main] with its extensions takes every aprond flag, plus the
// extensions' own (see [Configurable]):
//
//	package main
//
//	import (
//		"github.com/apron-chat/apron-server-go/aprond"
//		"example.com/weather"
//	)
//
//	func main() { aprond.Main(&weather.Extension{}) }
//
// cmd/aprond is that program with no extensions. A program that has its own
// flags, listeners, or lifecycle uses package apron directly instead, and
// serves apron.Server.Handler however it likes.
package aprond
