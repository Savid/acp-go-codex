//go:build darwin || freebsd

package homelock

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBSDLockFilesystemValidationBranches(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "lock-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	original := bsdFstatfs
	t.Cleanup(func() { bsdFstatfs = original })
	want := errors.New("statfs failed")
	bsdFstatfs = func(int, *unix.Statfs_t) error { return want }
	if err := validateLockFilesystem(file); !errors.Is(err, want) {
		t.Fatalf("statfs error = %v", err)
	}

	bsdFstatfs = func(_ int, stat *unix.Statfs_t) error {
		stat.Flags = 0
		copy(stat.Fstypename[:], "apfs")

		return nil
	}
	if err := validateLockFilesystem(file); err == nil {
		t.Fatal("remote filesystem was accepted")
	}

	bsdFstatfs = func(_ int, stat *unix.Statfs_t) error {
		stat.Flags = unix.MNT_LOCAL
		copy(stat.Fstypename[:], "network")

		return nil
	}
	if err := validateLockFilesystem(file); err == nil {
		t.Fatal("unknown local filesystem was accepted")
	}

	for _, name := range []string{"apfs", "hfs", "ufs", "zfs", "tmpfs"} {
		if !bsdLocalLockFilesystem(name) {
			t.Fatalf("approved filesystem %q was rejected", name)
		}
	}
	if bsdLocalLockFilesystem("network") {
		t.Fatal("unknown filesystem was approved")
	}
}
