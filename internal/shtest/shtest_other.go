//go:build !windows

package shtest

import "testing"

func interpreter(testing.TB) string { return "/bin/sh" }

func program(_ testing.TB, script string) string { return script }
