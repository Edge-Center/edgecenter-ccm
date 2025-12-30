package edgecenter

// cutString makes sure the string length doesn't exceed 63, which is usually the maximum string length in Edgecenter.
func cutString(original string) string {
	ret := original
	if len(original) > maxNameLength {
		ret = original[:maxNameLength]
	}
	return ret
}
