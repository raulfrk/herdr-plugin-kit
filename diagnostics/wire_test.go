package diagnostics

// RecordWireForTest exercises persisted-wire validation and retention without
// exposing free-form event ingestion in production.
func RecordWireForTest(recorder *Recorder, event Event) (Event, error) {
	return recorder.record(event)
}
