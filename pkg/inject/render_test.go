package inject

import (
	"testing"
)

func TestRender(t *testing.T) {
	tmplStr := string(envoyConfig)
	conf := GetEnvoyConfig(tmplStr, "test")
	t.Log(conf)
}
