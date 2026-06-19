package server

import "strconv"

// concurrencyFromFile reads the current maxbatchsize (allocation concurrency)
// from the downward-API labels file. ok=false if the file is unreadable or the
// label is absent/invalid.
func concurrencyFromFile(path string) (int, bool) {
	labels, err := ReadLabels(path)
	if err != nil {
		return 0, false
	}
	v, ok := labels[labelMaxBatchSize]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
