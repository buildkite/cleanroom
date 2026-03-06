//go:build !linux

package main

func ensureProcMounted() error {
	return nil
}
