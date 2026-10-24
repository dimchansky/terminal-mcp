package audit

// testSink is constructed before newLog launches its worker.
type testSink struct {
	write  func([]byte) error
	finish func() error
}

func (s *testSink) appendLine(data []byte) error {
	if s.write == nil {
		return nil
	}
	return s.write(data)
}
func (s *testSink) close() error {
	if s.finish == nil {
		return nil
	}
	return s.finish()
}
