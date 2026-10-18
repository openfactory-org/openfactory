package runnerapi

// ObjectKey returns the object key of an upload ID that this service issued.
// It checks the MAC of the ID. The control plane uses it to find the object of
// an artifact or transcript event.
func (s *Service) ObjectKey(uploadID string) (string, bool) {
	return s.parseUploadID(uploadID)
}
