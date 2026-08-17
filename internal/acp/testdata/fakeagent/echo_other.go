//go:build !unix

package main

func echoCommand() string        { return "cmd" }
func echoArgs(s string) []string { return []string{"/c", "echo", s} }
