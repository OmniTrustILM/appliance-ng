package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	size, free, err := Usage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 || free > size {
		t.Errorf("size %d, free %d", size, free)
	}
}

func TestUsageOfMissingPath(t *testing.T) {
	if _, _, err := Usage(t.TempDir() + "/missing"); err == nil {
		t.Error("no error for a missing path")
	}
}

func TestCheckAllowsZeroExpected(t *testing.T) {
	if err := Check(t.TempDir(), 0); err != nil {
		t.Errorf("Check(0) = %v", err)
	}
	if err := Check("/does/not/exist", 0); err != nil {
		t.Errorf("Check(0) on a missing path = %v", err)
	}
}

func TestCheckRefusesBelowTwiceExpected(t *testing.T) {
	dir := t.TempDir()
	_, free, err := Usage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(dir, 1); err != nil && free >= 2 {
		t.Errorf("Check(1) = %v with %d free", err, free)
	}
	// More than half of the free space expected: twice as much is not there.
	err = Check(dir, free/2+1)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Check(free/2+1) = %v, want ErrNoSpace", err)
	}
	if !strings.Contains(err.Error(), "free") {
		t.Errorf("error %q names no sizes", err)
	}
	if err := Check(dir, free/2); err != nil {
		t.Errorf("Check(free/2) = %v, want nil", err)
	}
}

func TestCheckReportsAMissingPath(t *testing.T) {
	err := Check(t.TempDir()+"/missing", 1)
	if err == nil || errors.Is(err, ErrNoSpace) {
		t.Errorf("Check on a missing path = %v", err)
	}
}
