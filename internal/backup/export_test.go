package backup

// SetMaxArchiveLimitsForTest overrides decompression limits for unit tests and returns a restore function.
func SetMaxArchiveLimitsForTest(entrySize, totalSize int64) func() {
	origEntry, origTotal := maxArchiveEntrySize, maxArchiveTotalSize
	maxArchiveEntrySize, maxArchiveTotalSize = entrySize, totalSize
	return func() {
		maxArchiveEntrySize, maxArchiveTotalSize = origEntry, origTotal
	}
}
