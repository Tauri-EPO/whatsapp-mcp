//go:build !unix

package main

// Windows permissions are governed by ACLs rather than a POSIX creation mask.
func privateProcessUmask() {}
