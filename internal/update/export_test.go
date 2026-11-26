package update

// Running tells whether this manager runs an update.
func (j *Jobs) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}
