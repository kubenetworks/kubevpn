package mesh

import (
	v1 "k8s.io/api/core/v1"
)

type Injectable interface {
	Inject() (map[string]string, *v1.PodSpec, error)
	Cancel() error
}
