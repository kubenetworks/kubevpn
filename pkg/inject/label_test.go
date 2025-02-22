package inject

import (
	"testing"

	"k8s.io/apimachinery/pkg/labels"
)

func TestLabelMatch(t *testing.T) {
	selector := map[string]string{
		"app":                    "universer",
		"app.kubernetes.io/name": "universer",
	}
	podLabels := map[string]string{
		"app":                    "universer",
		"app.kubernetes.io/name": "universer",
		"version":                "ai-chat-flow",
	}
	t.Log(labels.SelectorFromSet(selector).Matches(labels.Set(podLabels)))
}
