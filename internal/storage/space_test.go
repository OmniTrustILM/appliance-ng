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

func TestEnoughNeedsTwiceExpected(t *testing.T) {
	const most = ^uint64(0)
	for _, tc := range []struct {
		free, expected uint64
		want           bool
	}{
		{free: 200, expected: 100, want: true},
		{free: 199, expected: 100, want: false},
		{free: 201, expected: 100, want: true},
		{free: 0, expected: 1, want: false},
		{free: 2, expected: 1, want: true},
		{free: most, expected: most / 2, want: true},
		{free: most, expected: most/2 + 1, want: false},
		{free: most, expected: most, want: false},
	} {
		if got := enough(tc.free, tc.expected); got != tc.want {
			t.Errorf("enough(%d, %d) = %t, want %t", tc.free, tc.expected, got, tc.want)
		}
	}
}

func TestCheckRefusesMoreThanTheVolumeHolds(t *testing.T) {
	dir := t.TempDir()
	if err := Check(dir, 1); err != nil {
		t.Errorf("Check(1) = %v", err)
	}
	err := Check(dir, ^uint64(0)/2+1)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Check(an absurd size) = %v, want ErrNoSpace", err)
	}
	if !strings.Contains(err.Error(), "free") {
		t.Errorf("error %q names no sizes", err)
	}
}

func TestCheckReportsAMissingPath(t *testing.T) {
	err := Check(t.TempDir()+"/missing", 1)
	if err == nil || errors.Is(err, ErrNoSpace) {
		t.Errorf("Check on a missing path = %v", err)
	}
}
