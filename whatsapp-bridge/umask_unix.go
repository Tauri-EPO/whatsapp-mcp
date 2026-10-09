//go:build unix

package main

import "syscall"

// main calls this once, before any files or worker goroutines are created.
func privateProcessUmask() { syscall.Umask(0o077) }
