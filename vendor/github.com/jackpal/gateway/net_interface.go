package gateway

import "net"

// Wrapper for calls into the go "net" library that can be mocked for tests
type interfaceGetter interface {
	InterfaceByName(name string) (*net.Interface, error)
	Addrs(iface *net.Interface) ([]net.Addr, error)
}

// Concrete implementation of above interface
type intefaceGetterImpl struct{}

func (*intefaceGetterImpl) InterfaceByName(name string) (*net.Interface, error) {
	return net.InterfaceByName(name)
}

func (*intefaceGetterImpl) Addrs(iface *net.Interface) ([]net.Addr, error) {
	return iface.Addrs()
}
