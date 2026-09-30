package engine

// checkpointFileIdentity identifies a regular file and its hardlink count.
// An unavailable identity is an error; it must never become a fabricated
// single-link inode during checkpointing or restoration.
type checkpointFileIdentity struct {
	Nlink uint64
	Dev   uint64
	Ino   uint64
}

func (id checkpointFileIdentity) sameFile(other checkpointFileIdentity) bool {
	return id.Ino != 0 && other.Ino != 0 && id.Dev == other.Dev && id.Ino == other.Ino
}
