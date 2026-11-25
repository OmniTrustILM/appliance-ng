//go:build !linux

package firstboot

// SwitchVT does nothing: virtual terminals exist on Linux only.
func SwitchVT(int) error { return nil }
