package dns

import (
	"fmt"
	"testing"
)

func TestFix(t *testing.T) {
	domain := "authors"
	search := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	result := fix(domain, search)
	for _, s := range result {
		fmt.Println(s)
	}
}
