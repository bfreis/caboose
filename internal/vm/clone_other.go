//go:build !darwin

package vm

func clonefile(src, dst string) error { return errNoClone }
