// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import "fmt"

// noteLowDisk is the filesystem's report that the save folder's disk crossed
// its floor (low true) or has space again. The state line carries it, and the
// app tells the person once per crossing. Reads that need bytes from the peer
// fail with ENOSPC while it holds; what the peer sends is not affected until
// the disk is full for real.
func (kd *KeibiDrop) noteLowDisk(low bool, free uint64) {
	kd.diskLow.Store(low)
	if low {
		kd.logger.Warn("Save folder disk is almost full, reads from the peer stop", "free_mb", free>>20)
		kd.emitEvent(fmt.Sprintf("disk_low:%d", free>>20))
		return
	}
	kd.logger.Info("Save folder disk has space again", "free_mb", free>>20)
	kd.emitEvent("disk_ok:")
}

// DiskLow reports the save folder's disk under the floor at the last fetch.
func (kd *KeibiDrop) DiskLow() bool { return kd.diskLow.Load() }
