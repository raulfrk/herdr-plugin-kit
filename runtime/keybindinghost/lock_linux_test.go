//go:build linux

package keybindinghost

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestTransactionLockMetadataPolicy(t *testing.T) {
	const uid = 1000
	valid := unix.Stat_t{Mode: unix.S_IFREG | 0o600, Nlink: 1, Uid: uid}
	if !validTransactionLock(valid, uid) {
		t.Fatal("valid private owner-controlled lock rejected")
	}
	for name, mutate := range map[string]func(*unix.Stat_t){
		"nonregular":  func(stat *unix.Stat_t) { stat.Mode = unix.S_IFDIR | 0o700 },
		"hard linked": func(stat *unix.Stat_t) { stat.Nlink = 2 },
		"wrong owner": func(stat *unix.Stat_t) { stat.Uid++ },
		"group mode":  func(stat *unix.Stat_t) { stat.Mode |= 0o040 },
		"other mode":  func(stat *unix.Stat_t) { stat.Mode |= 0o004 },
	} {
		t.Run(name, func(t *testing.T) {
			stat := valid
			mutate(&stat)
			if validTransactionLock(stat, uid) {
				t.Fatalf("invalid metadata accepted: %+v", stat)
			}
		})
	}
}
