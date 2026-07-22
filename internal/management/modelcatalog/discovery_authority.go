package modelcatalog

func (s *Service) managementAuthoritativeModelKeys() map[string]bool {
	rows, ownerKeys, _, ok := s.defaultMappedOwnerRows()
	if !ok {
		return nil
	}
	return mappedOwnerRowModelKeys(rows, ownerKeys)
}
