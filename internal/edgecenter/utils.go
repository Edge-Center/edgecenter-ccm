package edgecenter

import "fmt"

func intPtrEqual(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func intPtrStr(v *int) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *v)
}
