package main

// openFile is one regular file a process holds open (GOV-B42).
//
// It exists for one question doctor asks of the Phase 0 sampler: is the samples.jsonl it is
// writing to still the one on disk? Name alone cannot answer that. After a rename the
// descriptor reports the file's NEW name, so a sampler whose log was rotated shows
// samples.jsonl.1, and an unlinked file keeps whatever name it last had. The inode and the
// link count are what tell a live file from a replaced or deleted one.
type openFile struct {
	fd    string
	name  string
	ino   uint64
	nlink uint64
}
