//go:build !linux

package main

import "fmt"

func loadProcessIsolationConfig(string) (processIsolationConfig, error) {
	return processIsolationConfig{}, fmt.Errorf("explicit process isolation is supported only on linux")
}
