//go:build !linux

package vpnclient

func raiseNetAdmin() error { return nil }
