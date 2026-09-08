package semantic

// ValidateCandidateSet validates the signed semantic candidate graph without
// persisting anything. Transport adapters use it to classify malformed client
// payloads as 4xx before the commit service reaches its transactional path.
func ValidateCandidateSet(in CandidateSet) error {
	return validateCandidateSet(in)
}
