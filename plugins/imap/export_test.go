package imap

// SetBatch changes the size of the batches and how often the cursor is saved,
// so tests can cut a run between checkpoints. It returns a function that
// restores them.
func SetBatch(size, every int) func() {
	oldSize, oldEvery := batchSize, checkpointEvery
	batchSize, checkpointEvery = size, every
	return func() { batchSize, checkpointEvery = oldSize, oldEvery }
}
