package core

import (
	"sync"

	"github.com/wencaiwulue/kubevpn/config"
)

var (
	SPool = &sync.Pool{
		New: func() interface{} {
			return make([]byte, config.SmallBufferSize)
		},
	}
	MPool = &sync.Pool{
		New: func() interface{} {
			return make([]byte, config.MediumBufferSize)
		},
	}
	LPool = &sync.Pool{
		New: func() interface{} {
			return make([]byte, config.LargeBufferSize)
		},
	}
)
