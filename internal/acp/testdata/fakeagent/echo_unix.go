//go:build unix

package main

func echoCommand() string        { return "/bin/echo" }
func echoArgs(s string) []string { return []string{s} }
