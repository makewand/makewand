package backup

// SetMaxArchiveLimitsForTest overrides decompression limits for unit tests and returns a restore function.
func SetMaxArchiveLimitsForTest(entrySize, totalSize int64, entryCount ...int) func() {
	origEntry, origTotal, origCount := maxArchiveEntrySize, maxArchiveTotalSize, maxArchiveEntryCount
	maxArchiveEntrySize, maxArchiveTotalSize = entrySize, totalSize
	if len(entryCount) > 0 {
		maxArchiveEntryCount = entryCount[0]
	}
	return func() {
		maxArchiveEntrySize, maxArchiveTotalSize, maxArchiveEntryCount = origEntry, origTotal, origCount
	}
}
